package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type activationPolicy struct {
	Enabled            bool              `json:"enabled"`
	IncludeNewAccounts bool              `json:"include_new_accounts"`
	Natural            bool              `json:"natural"`
	Reset              bool              `json:"reset"`
	FiveHour           bool              `json:"five_hour"`
	Weekly             bool              `json:"weekly"`
	Model              string            `json:"model"`
	Accounts           map[string]bool   `json:"accounts"`
	AccountModels      map[string]string `json:"account_models"`
}

func defaultActivationPolicy() activationPolicy {
	return activationPolicy{Natural: true, Reset: true, FiveHour: true, Weekly: true, Accounts: map[string]bool{}, AccountModels: map[string]string{}}
}

func validateActivationPolicy(p activationPolicy) error {
	if len(p.Accounts) > 10000 || len(p.AccountModels) > 10000 {
		return errors.New("自动激活账号设置过多")
	}
	validKey := func(s string) bool { return s != "" && s == strings.TrimSpace(s) && len(s) <= 512 }
	validModel := func(s string) bool {
		return len(s) <= 256 && s == strings.TrimSpace(s) && !strings.ContainsAny(s, "\r\n\x00")
	}
	if !validModel(p.Model) {
		return errors.New("自动激活模型无效")
	}
	for k := range p.Accounts {
		if !validKey(k) {
			return errors.New("自动激活账号标识无效")
		}
	}
	for k, v := range p.AccountModels {
		if !validKey(k) || !validModel(v) {
			return errors.New("自动激活账号模型无效")
		}
	}
	return nil
}

func activationEnabled(p activationPolicy, a account) bool {
	if !p.Enabled || a.Provider != "codex" || a.Disabled {
		return false
	}
	for _, k := range []string{a.Name, a.ID, a.AuthIndex} {
		if v, ok := p.Accounts[k]; ok {
			return v
		}
	}
	return p.IncludeNewAccounts
}

func activationPreferredModel(p activationPolicy, a account) string {
	for _, k := range []string{a.Name, a.ID, a.AuthIndex} {
		if v, ok := p.AccountModels[k]; ok && v != "" {
			return v
		}
	}
	return p.Model
}

type activationModel struct {
	ID           string `json:"id"`
	LowestEffort string `json:"lowest_effort"`
}
type activationCallResult struct {
	Model        string `json:"model"`
	Reasoning    string `json:"reasoning"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Confirmed    bool   `json:"confirmed"`
	Ambiguous    bool   `json:"ambiguous"`
}
type activationEvent struct {
	ID           string    `json:"id"`
	Account      string    `json:"account"`
	At           time.Time `json:"at"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	Windows      []string  `json:"windows"`
	Model        string    `json:"model,omitempty"`
	Reasoning    string    `json:"reasoning,omitempty"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Error        string    `json:"error,omitempty"`
	Confirmed    bool      `json:"confirmed"`
}
type activationAccountStatus struct {
	Enabled    bool              `json:"enabled"`
	State      string            `json:"state"`
	Message    string            `json:"message"`
	NextAt     time.Time         `json:"next_at,omitempty"`
	LastResult *activationEvent  `json:"last_result,omitempty"`
	Models     []activationModel `json:"models"`
}
type activationWindow struct {
	Bucket      bucket    `json:"bucket"`
	CheckedAt   time.Time `json:"checked_at"`
	Cycle       string    `json:"cycle"`
	Handled     bool      `json:"handled"`
	Boundary    int64     `json:"boundary"`
	ActiveUntil time.Time `json:"active_until,omitempty"`
	Reason      string    `json:"reason"`
}
type activationIdentity struct {
	Windows       map[string]activationWindow `json:"windows"`
	Pending       string                      `json:"pending,omitempty"`
	ResetSignal   string                      `json:"reset_signal,omitempty"`
	ResetSignalAt time.Time                   `json:"reset_signal_at,omitempty"`
	AppliedReset  string                      `json:"applied_reset,omitempty"`
}
type activationOperation struct {
	activationEvent
	Identity   string            `json:"identity"`
	Cycles     map[string]string `json:"cycles"`
	NoSend     bool              `json:"no_send,omitempty"`
	RetryAt    time.Time         `json:"retry_at,omitempty"`
	Preference string            `json:"preference,omitempty"`
}
type activationJournal struct {
	Version    int                            `json:"version"`
	Identities map[string]*activationIdentity `json:"identities"`
	Operations map[string]activationOperation `json:"operations"`
	Events     []activationEvent              `json:"events"`
}

func emptyActivationJournal() activationJournal {
	return activationJournal{Version: 1, Identities: map[string]*activationIdentity{}, Operations: map[string]activationOperation{}, Events: []activationEvent{}}
}

func loadActivationJournal(dir string) (activationJournal, error) {
	j := emptyActivationJournal()
	raw, err := os.ReadFile(filepath.Join(dir, "activation-journal.json"))
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err = decodeJSON(raw, &j); err != nil || j.Version != 1 || j.Identities == nil || j.Operations == nil {
		return emptyActivationJournal(), errors.New("自动激活执行记录无效")
	}
	for id, s := range j.Identities {
		if len(id) != 64 || s == nil || s.Windows == nil {
			return emptyActivationJournal(), errors.New("自动激活周期记录无效")
		}
		if s.Pending != "" {
			if _, ok := j.Operations[s.Pending]; !ok {
				return emptyActivationJournal(), errors.New("自动激活任务记录缺失")
			}
		}
	}
	for id, op := range j.Operations {
		if id != op.ID || len(id) != 64 || op.Identity == "" || len(op.Windows) == 0 || op.Cycles == nil || j.Identities[op.Identity] == nil {
			return emptyActivationJournal(), errors.New("自动激活请求记录无效")
		}
		switch op.State {
		case "pending", "sending", "succeeded", "uncertain", "failed", "skipped":
		default:
			return emptyActivationJournal(), errors.New("自动激活请求状态无效")
		}
		for _, k := range op.Windows {
			if (k != "five_hour" && k != "weekly") || op.Cycles[k] == "" {
				return emptyActivationJournal(), errors.New("自动激活请求周期缺失")
			}
		}
		if op.State == "sending" { // A crash can occur after the upstream accepted the one request.
			op.State = "uncertain"
			op.At = time.Now()
			op.Error = "上次进程在请求途中结束；可能已执行，不再自动重发"
			j.Operations[id] = op
			j.Events = append(j.Events, op.activationEvent)
		}
	}
	return j, nil
}

func writeActivationJournal(dir string, j activationJournal) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".activation-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "activation-journal.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (e *engine) initActivation() {
	j, err := loadActivationJournal(e.cfg.StateDir)
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	e.activationJournal = j
	e.activationError = ""
	if err != nil {
		e.activationError = "自动激活执行记录读取失败；已暂停自动调用"
	}
	e.activationStatuses = map[string]activationAccountStatus{}
	e.activationIdentities = map[string]string{}
	e.activationModelAt = map[string]time.Time{}
	if e.activationWake == nil {
		e.activationWake = make(chan struct{}, 1)
	}
	// Loading must not write: during hot reconfiguration the retiring engine may
	// still own an in-flight request. configure reloads after that worker stops.
}

func (e *engine) commitActivationLocked() error {
	if e.activationError != "" {
		return errors.New(e.activationError)
	}
	if len(e.activationJournal.Events) > 200 {
		e.activationJournal.Events = e.activationJournal.Events[len(e.activationJournal.Events)-200:]
	}
	if err := writeActivationJournal(e.cfg.StateDir, e.activationJournal); err != nil {
		e.activationError = "无法持久化自动激活记录；已暂停自动调用，避免重复消耗"
		return errors.New(e.activationError)
	}
	return nil
}

func (e *engine) wakeActivation() {
	select {
	case e.activationWake <- struct{}{}:
	default:
	}
}
func activationKind(b bucket) string {
	if b.Auxiliary {
		return ""
	}
	switch b.WindowSeconds {
	case 18000:
		return "five_hour"
	case 604800:
		return "weekly"
	}
	return ""
}
func activationSelected(p activationPolicy, k string) bool {
	return k == "five_hour" && p.FiveHour || k == "weekly" && p.Weekly
}
func activationCycleSelected(p activationPolicy, k, cycle string) bool {
	if !activationSelected(p, k) {
		return false
	}
	if strings.HasPrefix(cycle, "reset:") || strings.HasPrefix(cycle, "observed-reset:") {
		return p.Reset
	}
	return p.Natural
}
func activationIdle(b bucket, now time.Time) bool {
	return b.RemainingPercent >= 99.999999 && (b.ResetAt.IsZero() || !b.ResetAt.After(now))
}
func activationHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func activationAccountIdentity(host hostCall, a account) (string, error) {
	c, err := accountCredentials(host, a)
	if err != nil {
		return "", err
	}
	id := stringAt(c, "account_id")
	if id == "" {
		return "", errors.New("缺少上游 account_id，无法安全去重；自动激活已暂停")
	}
	return activationHash("codex\x00" + id), nil
}

func (e *engine) activationCurrentAccount(a account, identity string) (account, error) {
	var inventory struct {
		Files []account `json:"files"`
	}
	if e.host == nil || e.host("host.auth.list", map[string]any{}, &inventory) != nil {
		return account{}, errors.New("无法核实当前账号状态，暂缓自动调用")
	}
	for _, current := range inventory.Files {
		if current.ID == "" {
			current.ID = current.Name
		}
		if current.ID != a.ID {
			continue
		}
		if current.Disabled || current.Provider != "codex" || current.AuthIndex != a.AuthIndex {
			return account{}, errors.New("账号已停用或授权已变化，取消本次自动调用")
		}
		actual, err := activationAccountIdentity(e.host, current)
		if err != nil || actual != identity {
			return account{}, errors.New("账号身份已变化，取消本次自动调用")
		}
		return current, nil
	}
	return account{}, errors.New("账号已移除，取消本次自动调用")
}

// Called with activationMu, never with the policy lock. Only fresh provider data
// may open a cycle; merely refreshing a page cannot create an activation.
func (e *engine) observeActivationLocked(identity string, s snapshot, p activationPolicy, now time.Time) {
	e.activationIdentities[s.Account.ID] = identity
	view := e.activationStatuses[s.Account.ID]
	if view.State == "blocked" {
		view.State = ""
		view.Message = ""
		view.NextAt = time.Time{}
		e.activationStatuses[s.Account.ID] = view
	}
	st := e.activationJournal.Identities[identity]
	if st == nil {
		st = &activationIdentity{Windows: map[string]activationWindow{}}
		e.activationJournal.Identities[identity] = st
	}
	applyReset := p.Reset && st.ResetSignal != "" && st.AppliedReset != st.ResetSignal && !s.CheckedAt.Before(st.ResetSignalAt)
	resetReady := applyReset
	for _, b := range s.Buckets {
		if activationKind(b) != "" && b.RemainingPercent < 99.999999 {
			resetReady = false
		}
	}
	for _, b := range s.Buckets {
		k := activationKind(b)
		if k == "" {
			continue
		}
		w, exists := st.Windows[k]
		if exists && !s.CheckedAt.After(w.CheckedAt) {
			continue
		}
		idle := activationIdle(b, now)
		if !exists {
			w = activationWindow{Cycle: "initial:" + k, Handled: true}
			if idle && p.Natural && activationSelected(p, k) {
				w.Handled = false
				w.Reason = "首次启用时窗口满额且尚未启动"
			}
		}
		if exists && idle {
			old := w.Bucket
			boundary := w.ActiveUntil.Unix()
			natural := !w.ActiveUntil.IsZero() && !w.ActiveUntil.After(now) && w.Boundary != boundary
			manual := old.RemainingPercent < 99.999999 || old.ResetAt.After(now)
			if natural && p.Natural {
				w.Cycle = "natural:" + fmt.Sprint(boundary)
				w.Handled = false
				w.Reason = "自然额度刷新后的窗口激活"
				w.Boundary = boundary
			} else if manual && p.Reset {
				// An observed used-to-full transition before expiry is an external/manual reset.
				if old.ResetAt.IsZero() || old.ResetAt.After(now) {
					w.Cycle = "observed-reset:" + fmt.Sprint(w.ActiveUntil.Unix())
					w.Handled = false
					w.Reason = "检测到手动重置后的满额窗口"
					if !w.ActiveUntil.IsZero() {
						w.Boundary = w.ActiveUntil.Unix()
					}
				}
			}
		}
		if resetReady && idle {
			w.Cycle = "reset:" + st.ResetSignal
			w.Handled = false
			w.Reason = "重置卡使用成功后的窗口激活"
			if !w.ActiveUntil.IsZero() {
				w.Boundary = w.ActiveUntil.Unix()
			}
		}
		if !idle {
			w.Handled = true // Business traffic already started the new window.
			if b.ResetAt.After(now) && (w.ActiveUntil.IsZero() || b.ResetAt.Sub(w.ActiveUntil) > 5*time.Second || w.ActiveUntil.Sub(b.ResetAt) > 5*time.Second) {
				w.ActiveUntil = b.ResetAt
			}
		}
		w.Bucket = b
		w.CheckedAt = s.CheckedAt
		st.Windows[k] = w
	}
	if resetReady {
		st.AppliedReset = st.ResetSignal
	}
	e.queueActivationLocked(identity, s.Account, p, now)
	for id, op := range e.activationJournal.Operations {
		if op.Identity != identity || op.State != "succeeded" || op.Confirmed {
			continue
		}
		confirmed := true
		for _, k := range op.Windows {
			w, ok := st.Windows[k]
			if !ok || !w.CheckedAt.After(op.At) || !w.Bucket.ResetAt.After(now) {
				confirmed = false
			}
		}
		if confirmed {
			op.Confirmed = true
			e.activationJournal.Operations[id] = op
			for i := range e.activationJournal.Events {
				if e.activationJournal.Events[i].ID == id {
					e.activationJournal.Events[i].Confirmed = true
				}
			}
		}
	}
}

func (e *engine) queueActivationLocked(identity string, a account, p activationPolicy, now time.Time) {
	st := e.activationJournal.Identities[identity]
	windows := []string{}
	cycles := map[string]string{}
	reason := ""
	for k, w := range st.Windows {
		if activationCycleSelected(p, k, w.Cycle) && !w.Handled {
			windows = append(windows, k)
			cycles[k] = w.Cycle
			if reason == "" {
				reason = w.Reason
			}
		}
	}
	sort.Strings(windows)
	if st.Pending != "" {
		op := e.activationJournal.Operations[st.Pending]
		if op.State == "sending" {
			return
		}
		waiting := op.State == "pending" || op.State == "failed" && op.NoSend
		if waiting && len(windows) == 0 {
			op.State = "skipped"
			op.At = now
			op.Reason = "窗口已启动或对应来源已关闭，跳过自动调用"
			e.activationJournal.Operations[op.ID] = op
			e.activationJournal.Events = append(e.activationJournal.Events, op.activationEvent)
		}
		if !waiting || len(windows) == 0 {
			st.Pending = ""
		}
	}
	if len(windows) == 0 {
		return
	}
	key := identity
	for _, k := range windows {
		key += "\x00" + k + "\x00" + cycles[k]
	}
	id := activationHash(key)
	if op, ok := e.activationJournal.Operations[id]; ok && op.State != "pending" && !(op.State == "failed" && op.NoSend) {
		for _, k := range windows {
			w := st.Windows[k]
			w.Handled = true
			st.Windows[k] = w
		}
		st.Pending = ""
		return
	}
	if st.Pending != "" && st.Pending != id {
		old := e.activationJournal.Operations[st.Pending]
		old.State = "skipped"
		old.At = now
		old.Reason = "额度周期已更新，合并至新任务"
		e.activationJournal.Operations[old.ID] = old
	}
	if _, ok := e.activationJournal.Operations[id]; !ok {
		e.activationJournal.Operations[id] = activationOperation{activationEvent: activationEvent{ID: id, Account: a.Name, At: now, State: "pending", Reason: reason, Windows: windows}, Identity: identity, Cycles: cycles}
	}
	st.Pending = id
}

func (e *engine) observeActivations(ctx context.Context, snapshots []snapshot) {
	e.mu.Lock()
	p := e.p.AutoActivate
	e.mu.Unlock()
	seen := map[string]bool{}
	for _, s := range snapshots {
		if ctx.Err() != nil {
			return
		}
		a := s.Account
		if a.Provider != "codex" {
			continue
		}
		if !activationEnabled(p, a) {
			continue
		}
		if s.Error != "" || s.CheckedAt.IsZero() {
			e.setActivationStatus(a, "blocked", "额度查询失败或未知；等待成功查询", time.Time{})
			continue
		}
		identity, err := activationAccountIdentity(e.host, a)
		if err != nil {
			e.setActivationStatus(a, "blocked", err.Error(), time.Time{})
			continue
		}
		e.activationMu.Lock()
		e.activationIdentities[a.ID] = identity
		if !seen[identity] && e.activationError == "" {
			e.observeActivationLocked(identity, s, p, time.Now())
			_ = e.commitActivationLocked()
			seen[identity] = true
		}
		e.activationMu.Unlock()
	}
	e.wakeActivation()
}

func (e *engine) setActivationStatus(a account, state, message string, next time.Time) {
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	v := e.activationStatuses[a.ID]
	v.State = state
	v.Message = message
	v.NextAt = next
	e.activationStatuses[a.ID] = v
}

// This is also called while resetMu is held, so it must never take e.mu.
func (e *engine) markActivationReset(identity, key string, at time.Time) {
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	if e.activationError != "" {
		return
	}
	st := e.activationJournal.Identities[identity]
	if st == nil {
		st = &activationIdentity{Windows: map[string]activationWindow{}}
		e.activationJournal.Identities[identity] = st
	}
	if st.ResetSignal == key {
		return
	}
	st.ResetSignal = key
	st.ResetSignalAt = at
	_ = e.commitActivationLocked()
	e.wakeActivation()
}

// Only local cached state is read by status; model discovery happens in the background.
func (e *engine) activationView(a account, p activationPolicy) (*activationAccountStatus, []activationEvent, string) {
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	events := append([]activationEvent{}, e.activationJournal.Events...)
	if a.ID == "" {
		return nil, events, e.activationError
	}
	v := e.activationStatuses[a.ID]
	v.Enabled = activationEnabled(p, a)
	v.Models = append([]activationModel{}, v.Models...)
	if !v.Enabled {
		v.State = "disabled"
		v.Message = "自动激活已关闭"
	} else if e.activationError != "" {
		v.State = "blocked"
		v.Message = e.activationError
	} else if st := e.activationJournal.Identities[e.activationIdentities[a.ID]]; st != nil {
		for _, op := range e.activationJournal.Operations {
			if op.Identity == e.activationIdentities[a.ID] && (v.LastResult == nil || op.At.After(v.LastResult.At)) && op.State != "pending" {
				copy := op.activationEvent
				v.LastResult = &copy
			}
		}
		if st.Pending != "" {
			op := e.activationJournal.Operations[st.Pending]
			if v.State != "blocked" {
				v.State = op.State
				v.Message = op.Reason
				v.NextAt = op.RetryAt
			}
		} else if v.State != "blocked" && v.LastResult != nil {
			v.State = v.LastResult.State
			v.Message = v.LastResult.Reason
			if v.LastResult.Error != "" {
				v.Message = v.LastResult.Error
			}
		}
		if p.Natural {
			for k, w := range st.Windows {
				if activationSelected(p, k) && !w.ActiveUntil.IsZero() && w.ActiveUntil.Unix() != w.Boundary {
					next := w.ActiveUntil.Add(time.Second)
					if v.NextAt.IsZero() || next.Before(v.NextAt) {
						v.NextAt = next
					}
				}
			}
		}
	}
	if v.State == "" {
		v.State = "waiting"
		v.Message = "等待额度刷新或首次启动窗口"
	}
	return &v, events, e.activationError
}

func activationQuotaReady(s snapshot, p policy, now time.Time) error {
	if s.Account.Disabled || s.Error != "" || s.CheckedAt.IsZero() || now.Sub(s.CheckedAt) > time.Duration(p.MaxAgeSeconds)*time.Second {
		return errors.New("额度状态未知或过期，暂缓自动调用")
	}
	normal := 0
	for _, b := range s.Buckets {
		if b.Auxiliary {
			continue
		}
		normal++
		if !validPercent(b.RemainingPercent) || activationKind(b) == "" {
			return errors.New("存在未知额度窗口，暂缓自动调用")
		}
		if b.RemainingPercent <= 0 {
			return errors.New("另一个额度窗口已耗尽，暂缓自动调用")
		}
	}
	if normal == 0 {
		return errors.New("没有可判断的额度窗口")
	}
	return nil
}

func (e *engine) activationDelay(now time.Time) time.Duration {
	e.mu.Lock()
	p := e.p.AutoActivate
	e.mu.Unlock()
	if !p.Enabled || !p.Natural {
		return time.Minute
	}
	delay := time.Minute
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	for _, st := range e.activationJournal.Identities {
		for k, w := range st.Windows {
			if !activationSelected(p, k) || w.ActiveUntil.IsZero() || w.Handled && w.ActiveUntil.Unix() == w.Boundary {
				continue
			}
			d := w.ActiveUntil.Add(time.Second).Sub(now)
			if d < time.Second {
				d = 10 * time.Second
			}
			if d < delay {
				delay = d
			}
		}
	}
	return delay
}

func (e *engine) runActivationWorker(ctx context.Context) {
	for ctx.Err() == nil {
		e.processActivations(ctx)
		timer := time.NewTimer(e.activationDelay(time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.activationWake:
			timer.Stop()
		case <-timer.C:
			// The normal quota worker may use a long poll interval; verify a due
			// boundary independently rather than waiting for its next full poll.
			e.refreshDueActivations(ctx)
		}
	}
}

func (e *engine) runActivationCatalogWorker(ctx context.Context) {
	for ctx.Err() == nil {
		e.mu.Lock()
		cfg := e.cfg
		accounts := []account{}
		for _, s := range e.snapshots {
			if s.Account.Provider == "codex" && !s.Account.Disabled {
				accounts = append(accounts, s.Account)
			}
		}
		e.mu.Unlock()
		sem := make(chan struct{}, 2)
		var wg sync.WaitGroup
		for _, a := range accounts {
			e.activationMu.Lock()
			due := time.Since(e.activationModelAt[a.ID]) >= 5*time.Minute
			e.activationMu.Unlock()
			if !due {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(a account) {
				defer wg.Done()
				defer func() { <-sem }()
				models, err := activationModels(e.host, cfg, a)
				e.activationMu.Lock()
				defer e.activationMu.Unlock()
				v := e.activationStatuses[a.ID]
				if err == nil {
					v.Models = models
					e.activationModelAt[a.ID] = time.Now()
				} else {
					e.activationModelAt[a.ID] = time.Now().Add(-4 * time.Minute)
				}
				e.activationStatuses[a.ID] = v
			}(a)
		}
		wg.Wait()
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (e *engine) activationAccounts() (policy, runtimeConfig, []account) {
	e.mu.Lock()
	defer e.mu.Unlock()
	accounts := []account{}
	for _, s := range e.snapshots {
		if activationEnabled(e.p.AutoActivate, s.Account) {
			accounts = append(accounts, s.Account)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return e.p, e.cfg, accounts
}

func (e *engine) refreshDueActivations(ctx context.Context) {
	p, cfg, accounts := e.activationAccounts()
	now := time.Now()
	seen := map[string]bool{}
	if !p.AutoActivate.Natural {
		return
	}
	for _, a := range accounts {
		e.activationMu.Lock()
		identity := e.activationIdentities[a.ID]
		st := e.activationJournal.Identities[identity]
		due := false
		if st != nil {
			for k, w := range st.Windows {
				if activationSelected(p.AutoActivate, k) && !w.ActiveUntil.IsZero() && !w.ActiveUntil.After(now) && w.Boundary != w.ActiveUntil.Unix() {
					due = true
				}
			}
		}
		e.activationMu.Unlock()
		if !due || seen[identity] {
			continue
		}
		seen[identity] = true
		_, _ = e.activationFreshQuota(ctx, cfg, p, a)
	}
}

func (e *engine) activationFreshQuota(ctx context.Context, cfg runtimeConfig, p policy, a account) (snapshot, error) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	buckets, err := probeAccount(probeCtx, e.host, cfg, a)
	if err != nil {
		return snapshot{}, err
	}
	s := snapshot{Account: a, Buckets: buckets, CheckedAt: time.Now(), AttemptAt: time.Now()}
	e.mu.Lock()
	latest, exists := e.snapshots[a.ID]
	if !exists || latest.Account.Disabled || latest.Account.AuthIndex != a.AuthIndex || latest.Account.Provider != a.Provider {
		err = errors.New("账号状态已变化，取消本次额度核实")
	} else {
		s.Account = latest.Account
		err = e.checkWindowContinuity(latest, buckets)
		if err == nil {
			e.snapshots[a.ID] = s
		}
	}
	e.mu.Unlock()
	if err != nil {
		return snapshot{}, err
	}
	identity, err := activationAccountIdentity(e.host, a)
	if err != nil {
		return snapshot{}, err
	}
	e.mu.Lock()
	p = e.p
	e.mu.Unlock()
	e.activationMu.Lock()
	if e.activationError == "" && activationEnabled(p.AutoActivate, s.Account) {
		e.observeActivationLocked(identity, s, p.AutoActivate, time.Now())
		_ = e.commitActivationLocked()
	}
	e.activationMu.Unlock()
	return s, nil
}

func (e *engine) processActivations(ctx context.Context) {
	p, cfg, accounts := e.activationAccounts()
	seen := map[string]bool{}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		e.activationMu.Lock()
		identity := e.activationIdentities[a.ID]
		st := e.activationJournal.Identities[identity]
		pending := st != nil && st.Pending != ""
		disabled := e.activationError != ""
		if pending {
			op := e.activationJournal.Operations[st.Pending]
			if op.NoSend && time.Now().Before(op.RetryAt) && op.Preference == activationPreferredModel(p.AutoActivate, a) {
				pending = false
			}
		}
		e.activationMu.Unlock()
		if !pending || disabled || seen[identity] {
			continue
		}
		seen[identity] = true
		lock := e.resetIdentityLock(identity)
		lock.Lock()
		e.processActivation(ctx, p, cfg, a, identity)
		lock.Unlock()
	}
}

func (e *engine) processActivation(ctx context.Context, p policy, cfg runtimeConfig, a account, identity string) {
	s, err := e.activationFreshQuota(ctx, cfg, p, a)
	if err != nil {
		e.setActivationStatus(a, "blocked", "调用前额度核实失败；等待下次成功查询", time.Now().Add(time.Minute))
		return
	}
	e.mu.Lock()
	p = e.p
	current, exists := e.snapshots[a.ID]
	guard := e.evaluate(a, time.Now())
	e.mu.Unlock()
	if !exists || !activationEnabled(p.AutoActivate, current.Account) {
		return
	}
	if err = activationQuotaReady(s, p, time.Now()); err != nil {
		e.setActivationStatus(a, "blocked", err.Error(), time.Now().Add(time.Minute))
		return
	}
	if guard.Protected {
		e.setActivationStatus(a, "blocked", "额度保护生效，暂缓自动调用："+guard.Reason, time.Now().Add(time.Minute))
		return
	}
	currentAccount, err := e.activationCurrentAccount(a, identity)
	if err != nil {
		e.setActivationStatus(a, "blocked", err.Error(), time.Now().Add(time.Minute))
		return
	}
	a = currentAccount
	e.mu.Lock()
	p = e.p
	enabled := activationEnabled(p.AutoActivate, a)
	guard = e.evaluate(a, time.Now())
	e.mu.Unlock()
	if !enabled || guard.Protected {
		return
	}
	e.activationMu.Lock()
	st := e.activationJournal.Identities[identity]
	if e.activationError != "" || st == nil || st.Pending == "" {
		e.activationMu.Unlock()
		return
	}
	op := e.activationJournal.Operations[st.Pending]
	if op.State != "pending" && !(op.State == "failed" && op.NoSend && (time.Now().After(op.RetryAt) || op.Preference != activationPreferredModel(p.AutoActivate, a))) {
		e.activationMu.Unlock()
		return
	}
	for _, k := range op.Windows {
		if !activationCycleSelected(p.AutoActivate, k, op.Cycles[k]) {
			e.activationMu.Unlock()
			return
		}
	}
	op.State = "sending"
	op.At = time.Now()
	op.NoSend = false
	op.RetryAt = time.Time{}
	op.Preference = activationPreferredModel(p.AutoActivate, a)
	e.activationJournal.Operations[op.ID] = op
	if e.commitActivationLocked() != nil {
		e.activationMu.Unlock()
		return
	}
	e.activationMu.Unlock()
	e.setActivationStatus(a, "sending", "正在发送一次最小额度模型请求", time.Time{})
	// No policy/journal lock is held across a host callback or HTTP request.
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	result, callErr := executeActivation(callCtx, e.host, cfg, a, activationPreferredModel(p.AutoActivate, a))
	cancel()
	op.Model = result.Model
	op.Reasoning = result.Reasoning
	op.InputTokens = result.InputTokens
	op.OutputTokens = result.OutputTokens
	op.At = time.Now()
	op.Error = ""
	op.State = "succeeded"
	if result.Ambiguous {
		op.State = "uncertain"
		op.Error = "请求结果不确定；可能已执行，不再自动重发"
	} else if callErr != nil || !result.Confirmed {
		op.State = "failed"
		op.Error = "自动激活模型请求失败"
	}
	if callErr != nil {
		op.Error = callErr.Error()
	}
	op.NoSend = op.State == "failed" && !result.Ambiguous
	if op.NoSend {
		op.RetryAt = time.Now().Add(5 * time.Minute)
	}
	e.activationMu.Lock()
	e.activationJournal.Operations[op.ID] = op
	for k, cycle := range op.Cycles {
		w := st.Windows[k]
		if w.Cycle == cycle {
			w.Handled = !op.NoSend
			st.Windows[k] = w
		}
	}
	if !op.NoSend {
		st.Pending = ""
	}
	e.activationJournal.Events = append(e.activationJournal.Events, op.activationEvent)
	_ = e.commitActivationLocked()
	e.activationMu.Unlock()
	e.setActivationStatus(a, op.State, op.Reason, op.RetryAt)
	if op.State == "succeeded" {
		// Read the resulting clocks for the UI; a successful request is never
		// retried merely because quota reporting is delayed.
		post, probeErr := e.activationFreshQuota(ctx, cfg, p, a)
		if probeErr == nil {
			confirmed := true
			for _, k := range op.Windows {
				found := false
				for _, b := range post.Buckets {
					if activationKind(b) == k && b.ResetAt.After(time.Now()) {
						found = true
					}
				}
				confirmed = confirmed && found
			}
			if confirmed {
				e.activationMu.Lock()
				latest := e.activationJournal.Operations[op.ID]
				latest.Confirmed = true
				e.activationJournal.Operations[op.ID] = latest
				for i := range e.activationJournal.Events {
					if e.activationJournal.Events[i].ID == op.ID {
						e.activationJournal.Events[i].Confirmed = true
					}
				}
				_ = e.commitActivationLocked()
				e.activationMu.Unlock()
			}
		}
	}
}
