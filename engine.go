package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type engine struct {
	mu                   sync.Mutex
	refreshMu            sync.Mutex
	cfg                  runtimeConfig
	p                    policy
	host                 hostCall
	snapshots            map[string]snapshot
	latches              map[string]bool
	cursors              map[string]string
	wake                 chan struct{}
	stop                 chan struct{}
	done                 chan struct{}
	lastRefreshError     string
	resetMu              sync.Mutex
	resetRefreshMu       sync.Mutex
	identityMu           sync.Mutex
	identityLocks        map[string]*sync.Mutex
	resetWake            chan struct{}
	probeRetry           map[string]retryState
	resetRetry           map[string]retryState
	health               engineHealth
	notificationMu       sync.Mutex
	notificationStatus   notificationStatus
	notificationAttempts map[string]notificationAttempt
	resetJournal         resetJournal
	resetJournalError    string
	resetSnapshots       map[string]resetAccountStatus
}

func newEngine(cfg runtimeConfig, host hostCall) (*engine, error) {
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/cliproxyapi/quota-guard"
	}
	if cfg.ManagementURL == "" {
		cfg.ManagementURL = "http://127.0.0.1:8317"
	}
	if err := validateRuntime(cfg); err != nil {
		return nil, err
	}
	p, err := loadPolicy(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	journal, journalErr := loadResetJournal(cfg.StateDir)
	e := &engine{cfg: cfg, p: p, host: host, snapshots: map[string]snapshot{}, latches: map[string]bool{}, cursors: map[string]string{}, wake: make(chan struct{}, 1), resetWake: make(chan struct{}, 1), identityLocks: map[string]*sync.Mutex{}, probeRetry: map[string]retryState{}, resetRetry: map[string]retryState{}, notificationAttempts: map[string]notificationAttempt{}, stop: make(chan struct{}), done: make(chan struct{}), resetJournal: journal, resetSnapshots: map[string]resetAccountStatus{}}
	if journalErr != nil {
		e.resetJournalError = "重置卡执行记录读取失败；自动使用已暂停，额度保护继续运行"
	}
	if cfg.NotificationWebhookFile != "" {
		e.notificationStatus = notificationStatus{Enabled: true, Health: "ready"}
		if _, err := readWebhook(cfg.NotificationWebhookFile); err != nil {
			e.notificationStatus.Health = "error"
			e.notificationStatus.LastError = err.Error()
		}
	}
	return e, nil
}

func (e *engine) run() {
	defer close(e.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); e.runQuotaWorker(ctx) }()
	go func() { defer workers.Done(); e.runResetWorker(ctx) }()
	go func() { defer workers.Done(); e.runNotificationWorker(ctx) }()
	<-e.stop
	cancel()
	workers.Wait()
}

// Synchronous refresh is also useful for isolated checks; scheduled workers run independently.
func (e *engine) refresh(ctx context.Context) error {
	if err := e.refreshQuota(ctx, true); err != nil {
		return err
	}
	return e.refreshResets(ctx, true)
}

func (e *engine) refreshQuota(ctx context.Context, force bool) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	e.mu.Lock()
	e.health.QuotaRefreshing = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.health.QuotaRefreshing = false; e.mu.Unlock() }()
	var result struct {
		Files []account `json:"files"`
	}
	if err := e.host("host.auth.list", map[string]any{}, &result); err != nil {
		e.mu.Lock()
		e.lastRefreshError = "无法读取 CPA 账号列表"
		e.mu.Unlock()
		return errors.New("无法读取 CPA 账号列表")
	}
	e.mu.Lock()
	p := e.p
	cfg := e.cfg
	e.mu.Unlock()
	seen := map[string]bool{}
	// At most four probes run at a time; a slow provider cannot block all other accounts.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, a := range result.Files {
		if a.ID == "" {
			a.ID = a.Name
		}
		if a.Provider == "" {
			continue
		}
		seen[a.ID] = true
		e.mu.Lock()
		old := e.snapshots[a.ID]
		old.Account = a
		retry := e.probeRetry[a.ID]
		e.snapshots[a.ID] = old
		e.mu.Unlock()
		if a.Disabled || (a.Provider != "codex" && a.Provider != "antigravity") || (!force && time.Now().Before(retry.NextAt)) {
			continue
		}
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
			probeCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
			defer cancel()
			buckets, err := probeAccount(probeCtx, e.host, cfg, a)
			now := time.Now()
			e.mu.Lock()
			defer e.mu.Unlock()
			s := e.snapshots[a.ID]
			s.AttemptAt = now
			if err == nil {
				err = e.checkWindowContinuity(s, buckets)
			}
			if err != nil {
				e.probeRetry[a.ID] = nextRetry(e.probeRetry[a.ID], now)
				s.Error = err.Error()
			} else {
				delete(e.probeRetry, a.ID)
				e.health.LastQuotaSuccessAt = now
				s.Buckets = buckets
				s.CheckedAt = now
				s.Error = ""
			}
			e.snapshots[a.ID] = s
		}(a)
	}
	wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.snapshots {
		if !seen[id] {
			delete(e.snapshots, id)
			delete(e.probeRetry, id)
			for key := range e.latches {
				if strings.HasPrefix(key, id+"\x00") {
					delete(e.latches, key)
				}
			}
		}
	}
	// Observe every window on each poll, even when there are no model requests or UI readers.
	for _, s := range e.snapshots {
		_ = e.evaluate(s.Account, time.Now())
	}
	e.lastRefreshError = ""
	return ctx.Err()
}

func (e *engine) notifyRefresh() {
	select {
	case e.resetWake <- struct{}{}:
	default:
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *engine) ruleFor(a account) *rule { return ruleForPolicy(e.p, a) }

func ruleForPolicy(p policy, a account) *rule {
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Account == a.Name || r.Account == a.ID || r.Account == a.AuthIndex {
			return r
		}
	}
	return nil
}

// Called under e.mu. The latch is updated only from fresh observations, never from a reset-time prediction.
func (e *engine) evaluate(a account, now time.Time) decision {
	r := e.ruleFor(a)
	if r == nil || !r.Enabled {
		return decision{Reason: "未启用额度保护"}
	}
	unknown := func(reason string) decision { return decision{Protected: e.p.UnknownAction == "block", Reason: reason} }
	if a.Provider != "codex" && a.Provider != "antigravity" {
		return unknown("该服务商尚无额度适配器")
	}
	s, ok := e.snapshots[a.ID]
	if !ok || s.CheckedAt.IsZero() {
		return unknown("尚无成功的额度查询")
	}
	if now.Sub(s.CheckedAt) > time.Duration(e.p.MaxAgeSeconds)*time.Second {
		return unknown("额度数据已过期")
	}
	selected := 0
	found := map[string]bool{}
	heldReason := ""
	for _, b := range s.Buckets {
		min, explicit := r.Windows[b.ID]
		if len(r.Windows) > 0 && !explicit {
			continue
		}
		if len(r.Windows) == 0 {
			if b.Auxiliary {
				continue
			}
			min = r.MinRemainingPercent
		}
		found[b.ID] = true
		selected++
		key := a.ID + "\x00" + b.ID
		cutoff := math.Min(100, min+e.p.SafetyPercent)
		resume := math.Min(100, cutoff+e.p.ResumeGapPercent)
		held := e.latches[key]
		if b.RemainingPercent <= cutoff {
			held = true
		} else if held && b.RemainingPercent >= resume {
			held = false
		}
		e.latches[key] = held
		if held && heldReason == "" {
			heldReason = fmt.Sprintf("%s 剩余 %.1f%%；保护线 %.1f%%，恢复线 %.1f%%", b.Label, b.RemainingPercent, cutoff, resume)
		}
	}
	// A known low window always protects the account, even if another selected window is missing.
	if heldReason != "" {
		return decision{Protected: true, Reason: heldReason}
	}
	for id := range r.Windows {
		if !found[id] {
			return unknown("指定额度窗口缺失: " + id)
		}
	}
	if selected == 0 {
		return unknown("没有可判断的额度窗口")
	}
	return decision{Reason: "额度充足"}
}

func (e *engine) pick(req pickRequest) pickResponse {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.p.Mode == "observe" {
		return pickResponse{}
	}
	eligible := make([]candidate, 0, len(req.Candidates))
	anyProtected := false
	now := time.Now()
	for _, c := range req.Candidates {
		a := account{ID: c.ID, Name: c.ID, Provider: strings.ToLower(c.Provider)}
		if s, ok := e.snapshots[c.ID]; ok {
			a = s.Account
		}
		if r := e.ruleFor(a); r != nil && r.Enabled {
			anyProtected = true
		}
		if !e.evaluate(a, now).Protected {
			eligible = append(eligible, c)
		}
	}
	// With no matching rules preserve native affinity, weighted routing and all other built-ins.
	if !anyProtected {
		return pickResponse{}
	}
	if len(eligible) == 0 {
		return pickResponse{Handled: true, Reject: true, RejectCode: "quota_guard_protected", RejectReason: "所有可用账号均达到额度保护阈值，或额度数据不可用"}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority > eligible[j].Priority
		}
		return eligible[i].ID < eligible[j].ID
	})
	best := eligible[0].Priority
	count := 0
	for count < len(eligible) && eligible[count].Priority == best {
		count++
	}
	eligible = eligible[:count]
	selected := eligible[0]
	if e.p.Strategy == "round-robin" {
		key := req.Provider + "\x00" + req.Model
		last := e.cursors[key]
		for _, c := range eligible {
			if c.ID > last {
				selected = c
				break
			}
		}
		// Bound memory even for callers supplying an unbounded set of model aliases.
		if len(e.cursors) > 4096 {
			e.cursors = map[string]string{}
		}
		e.cursors[key] = selected.ID
	}
	return pickResponse{Handled: true, AuthID: selected.ID}
}

func (e *engine) status() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	statuses := make([]accountStatus, 0, len(e.snapshots))
	for _, s := range e.snapshots {
		d := e.evaluate(s.Account, time.Now())
		reset, _, _ := e.resetView(s.Account)
		statuses = append(statuses, accountStatus{snapshot: s, decision: d, WouldBlock: d.Protected, Reset: reset, Health: quotaHealth(s, e.p, time.Now()), EffectiveRule: e.ruleFor(s.Account), Warnings: e.ruleWarnings(s.Account)})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Account.Name < statuses[j].Account.Name })
	_, events, resetError := e.resetView(account{})
	return struct {
		Version       string             `json:"version"`
		Policy        policy             `json:"policy"`
		Accounts      []accountStatus    `json:"accounts"`
		RefreshError  string             `json:"refresh_error,omitempty"`
		ResetEvents   []resetEvent       `json:"reset_events"`
		ResetError    string             `json:"reset_error,omitempty"`
		Build         buildInfo          `json:"build"`
		Health        engineHealth       `json:"health"`
		Notifications notificationStatus `json:"notifications"`
	}{version, e.p, statuses, e.lastRefreshError, events, resetError, buildInfo{version, buildCommit, buildTime, runtime.Version(), "0.3.0", "8.0.35 / C ABI 1 / schema 6", "1.26.5 (verified HTML build)"}, e.health, e.notificationsView()}
}

var errRevision = errors.New("配置已被其他页面修改，请刷新后重试")

func (e *engine) updatePolicy(p policy) error {
	if err := validatePolicy(p); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Revision != e.p.Revision {
		return errRevision
	}
	p.Revision++
	if err := savePolicy(e.cfg.StateDir, p); err != nil {
		return errors.New("保存策略失败；原策略继续生效，请检查 state_dir 写入权限")
	}
	previous := e.p
	e.p = p
	for key := range e.latches {
		id := strings.SplitN(key, "\x00", 2)[0]
		a := account{ID: id, Name: id}
		if snap, ok := e.snapshots[id]; ok {
			a = snap.Account
		}
		oldRule := ruleForPolicy(previous, a)
		newRule := e.ruleFor(a)
		if previous.SafetyPercent != p.SafetyPercent || previous.ResumeGapPercent != p.ResumeGapPercent || !reflect.DeepEqual(oldRule, newRule) {
			delete(e.latches, key)
		}
	}
	if previous.Strategy != p.Strategy {
		e.cursors = map[string]string{}
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	select {
	case e.resetWake <- struct{}{}:
	default:
	}
	return nil
}
