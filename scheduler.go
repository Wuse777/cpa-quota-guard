package main

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

type engineHealth struct {
	LastQuotaSuccessAt time.Time `json:"last_quota_success_at,omitempty"`
	LastResetSuccessAt time.Time `json:"last_reset_success_at,omitempty"`
	QuotaRefreshing    bool      `json:"quota_refreshing"`
	ResetRefreshing    bool      `json:"reset_refreshing"`
}
type buildInfo struct {
	Version            string `json:"version"`
	Commit             string `json:"commit"`
	BuiltAt            string `json:"built_at"`
	GoVersion          string `json:"go_version"`
	BridgeVersion      string `json:"bridge_version"`
	CPACompatibility   string `json:"cpa_compatibility"`
	CPAMCCompatibility string `json:"cpamc_compatibility"`
}
type retryState struct {
	Failures int
	NextAt   time.Time
}

func nextRetry(previous retryState, now time.Time) retryState {
	n := previous.Failures + 1
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	delay := 30 * time.Second * time.Duration(1<<shift)
	// Desynchronize failing accounts while keeping the retry bounded.
	delay += time.Duration(rand.Int63n(int64(delay/5 + 1)))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	return retryState{n, now.Add(delay)}
}

// Inventory failures must not sleep through a known redemption window. Keep
// ordinary exponential backoff outside the window, then cap it at one minute.
func resetQueryRetry(previous retryState, credits []resetCredit, p resetPolicy, enabled bool, now time.Time) retryState {
	retry := nextRetry(previous, now)
	if !enabled {
		return retry
	}
	for _, credit := range credits {
		if !credit.ExpiresAt.After(now) {
			continue
		}
		deadline := credit.ExpiresAt.Add(-time.Duration(p.BeforeHours * float64(time.Hour)))
		if !deadline.After(now) {
			deadline = now.Add(time.Minute)
		}
		if deadline.Before(retry.NextAt) {
			retry.NextAt = deadline
		}
	}
	return retry
}

func (e *engine) resetIdentityLock(identity string) *sync.Mutex {
	e.identityMu.Lock()
	defer e.identityMu.Unlock()
	lock := e.identityLocks[identity]
	if lock == nil {
		lock = &sync.Mutex{}
		e.identityLocks[identity] = lock
	}
	return lock
}

func (e *engine) runQuotaWorker(ctx context.Context) {
	force := false
	for ctx.Err() == nil {
		started := time.Now()
		_ = e.refreshQuota(ctx, force)
		e.mu.Lock()
		interval := time.Duration(e.p.PollSeconds) * time.Second
		e.mu.Unlock()
		delay := time.Until(started.Add(interval))
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.wake:
			timer.Stop()
			force = true
		case <-timer.C:
			force = false
		}
	}
}

func (e *engine) runResetWorker(ctx context.Context) {
	force := false
	for ctx.Err() == nil {
		started := time.Now()
		_ = e.refreshResets(ctx, force)
		e.mu.Lock()
		enabled := e.p.AutoReset.Enabled
		e.mu.Unlock()
		interval := 10 * time.Minute
		if enabled {
			interval = time.Minute
		}
		delay := time.Until(started.Add(interval))
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.resetWake:
			timer.Stop()
			force = true
		case <-timer.C:
			force = false
		}
	}
}

func (e *engine) refreshResets(ctx context.Context, force bool) error {
	e.resetRefreshMu.Lock()
	defer e.resetRefreshMu.Unlock()
	e.mu.Lock()
	e.health.ResetRefreshing = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.health.ResetRefreshing = false; e.mu.Unlock() }()
	var result struct {
		Files []account `json:"files"`
	}
	if err := e.host("host.auth.list", map[string]any{}, &result); err != nil {
		return errors.New("无法读取 CPA 账号列表")
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, a := range result.Files {
		if a.ID == "" {
			a.ID = a.Name
		}
		if a.Provider != "codex" || a.Disabled {
			continue
		}
		seen[a.ID] = true
		e.mu.Lock()
		retry := e.resetRetry[a.ID]
		e.mu.Unlock()
		if !force && time.Now().Before(retry.NextAt) {
			continue
		}
		// Acquire before spawning, so even a very large account list has bounded workers.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(a account) {
			defer wg.Done()
			defer func() { <-sem }()
			e.resetMu.Lock()
			before := e.resetSnapshots[a.ID].CheckedAt
			e.resetMu.Unlock()
			e.processReset(ctx, a)
			e.resetMu.Lock()
			current := e.resetSnapshots[a.ID]
			after := current.CheckedAt
			e.resetMu.Unlock()
			e.mu.Lock()
			defer e.mu.Unlock()
			if after.After(before) {
				delete(e.resetRetry, a.ID)
				e.health.LastResetSuccessAt = after
			} else if ctx.Err() == nil {
				e.resetRetry[a.ID] = resetQueryRetry(e.resetRetry[a.ID], current.Credits, e.p.AutoReset, resetEnabled(e.p.AutoReset, a), time.Now())
			}
		}(a)
	}
	wg.Wait()
	e.resetMu.Lock()
	for id := range e.resetSnapshots {
		if !seen[id] {
			delete(e.resetSnapshots, id)
		}
	}
	e.resetMu.Unlock()
	e.mu.Lock()
	for id := range e.resetRetry {
		if !seen[id] {
			delete(e.resetRetry, id)
		}
	}
	e.mu.Unlock()
	return ctx.Err()
}

// A provider response that silently loses a previously protected normal window
// cannot release a default rule. Explicit window selection can resolve plan changes.
func (e *engine) checkWindowContinuity(old snapshot, current []bucket) error {
	r := e.ruleFor(old.Account)
	if r == nil || !r.Enabled || len(r.Windows) > 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, b := range current {
		seen[b.ID] = true
	}
	for _, b := range old.Buckets {
		if !b.Auxiliary && !seen[b.ID] {
			return errors.New("已知额度窗口缺失：" + b.ID + "；保留上次成功快照，请检查上游响应或明确选择窗口")
		}
	}
	return nil
}

func quotaHealth(s snapshot, p policy, now time.Time) string {
	if s.Account.Disabled {
		return "disabled"
	}
	if s.Account.Provider != "codex" && s.Account.Provider != "antigravity" {
		return "unsupported"
	}
	if s.CheckedAt.IsZero() {
		if s.Error != "" {
			return "error"
		}
		return "unknown"
	}
	if now.Sub(s.CheckedAt) > time.Duration(p.MaxAgeSeconds)*time.Second {
		return "stale"
	}
	if s.Error != "" {
		return "error"
	}
	return "healthy"
}

func (e *engine) ruleWarnings(a account) []string {
	warnings := []string{}
	r := e.ruleFor(a)
	if r == nil || !r.Enabled {
		return warnings
	}
	if len(r.Windows) == 0 {
		if r.MinRemainingPercent+e.p.SafetyPercent >= 100 {
			warnings = append(warnings, "停止线为 100%，此规则将持续阻断该账号，即使额度已满")
		}
	} else {
		for id, min := range r.Windows {
			if min+e.p.SafetyPercent >= 100 {
				warnings = append(warnings, "窗口 "+id+" 的停止线为 100%，此规则将持续阻断该账号")
			}
		}
	}
	sort.Strings(warnings)
	return warnings
}

func resetSchedulerState(s resetAccountStatus, journalError string) (string, string) {
	if !s.Enabled {
		return "disabled", "自动使用已关闭"
	}
	if journalError != "" {
		return "blocked", journalError
	}
	if s.QueryError != "" {
		return "error", s.QueryError
	}
	if s.SchedulerState != "" {
		return s.SchedulerState, s.BlockedReason
	}
	if strings.Contains(s.Message, "暂停") {
		return "blocked", s.Message
	}
	return "waiting", ""
}
