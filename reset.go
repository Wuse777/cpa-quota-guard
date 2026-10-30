package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type resetCredit struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	GrantedAt time.Time `json:"granted_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}
type resetAccountStatus struct {
	Enabled    bool          `json:"enabled"`
	Credits    []resetCredit `json:"credits"`
	CheckedAt  time.Time     `json:"checked_at,omitempty"`
	AttemptAt  time.Time     `json:"attempt_at,omitempty"`
	NextAt     time.Time     `json:"next_at,omitempty"`
	Message    string        `json:"message"`
	Error      string        `json:"error,omitempty"`
	LastResult *resetResult  `json:"last_result,omitempty"`
}
type resetOperation struct {
	Account         string       `json:"account"`
	Identity        string       `json:"identity"`
	Target          string       `json:"target"`
	ExpiresAt       time.Time    `json:"expires_at"`
	BeforeIDs       []string     `json:"before_ids"`
	RequestID       string       `json:"request_id"`
	State           string       `json:"state"`
	Attempts        int          `json:"attempts"`
	UpdatedAt       time.Time    `json:"updated_at"`
	NextTryAt       time.Time    `json:"next_try_at,omitempty"`
	Message         string       `json:"message"`
	CooldownCleared bool         `json:"cooldown_cleared"`
	LastResult      *resetResult `json:"last_result,omitempty"`
}
type resetEvent struct {
	At      time.Time    `json:"at"`
	Account string       `json:"account"`
	Credit  string       `json:"credit"`
	State   string       `json:"state"`
	Message string       `json:"message"`
	Result  *resetResult `json:"result,omitempty"`
}
type resetJournal struct {
	Version    int                       `json:"version"`
	Operations map[string]resetOperation `json:"operations"`
	Events     []resetEvent              `json:"events"`
}

func loadResetJournal(dir string) (resetJournal, error) {
	j := resetJournal{Version: 1, Operations: map[string]resetOperation{}, Events: []resetEvent{}}
	raw, err := os.ReadFile(filepath.Join(dir, "reset-journal.json"))
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if json.Unmarshal(raw, &j) != nil || j.Version != 1 || j.Operations == nil {
		return j, errors.New("重置卡执行记录无效；自动使用已暂停，额度保护继续运行")
	}
	for key, op := range j.Operations {
		if op.RequestID == "" || op.Identity == "" || op.Target == "" || op.ExpiresAt.IsZero() {
			return j, errors.New("重置卡执行记录缺失关键字段；自动使用已暂停")
		}
		if op.LastResult == nil && op.Attempts > 0 && op.State != "success" && op.State != "confirmed" {
			op.LastResult = &resetResult{At: op.UpdatedAt, Error: "历史兑换未确认成功；旧版本未保存 HTTP 状态及上游错误：" + op.Message}
			j.Operations[key] = op
		}
	}
	return j, nil
}

func writeResetJournal(dir string, j resetJournal) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".reset-*.tmp")
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
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "reset-journal.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Called under resetMu. Commit the request ID before sending any consume request.
func (e *engine) commitReset(key string, op resetOperation, event bool) error {
	op.UpdatedAt = time.Now()
	e.resetJournal.Operations[key] = op
	if event {
		e.resetJournal.Events = append(e.resetJournal.Events, resetEvent{At: op.UpdatedAt, Account: op.Account, Credit: op.Target, State: op.State, Message: op.Message, Result: op.LastResult})
		if len(e.resetJournal.Events) > 200 {
			e.resetJournal.Events = e.resetJournal.Events[len(e.resetJournal.Events)-200:]
		}
	}
	// Retain terminal transaction IDs for thirty days after expiration.
	for k, old := range e.resetJournal.Operations {
		if time.Since(old.ExpiresAt) > 30*24*time.Hour && old.State != "pending" && old.State != "confirmed" {
			delete(e.resetJournal.Operations, k)
		}
	}
	if err := writeResetJournal(e.cfg.StateDir, e.resetJournal); err != nil {
		e.resetJournalError = "无法持久化重置卡执行记录；已暂停自动使用，防止重复兑换"
		return errors.New(e.resetJournalError)
	}
	return nil
}

func resetEnabled(p resetPolicy, a account) bool {
	if !p.Enabled || a.Provider != "codex" || a.Disabled {
		return false
	}
	for _, id := range []string{a.Name, a.ID, a.AuthIndex} {
		if enabled, ok := p.Accounts[id]; ok {
			return enabled
		}
	}
	return true
}

func parseResetCredits(raw []byte) ([]resetCredit, *int, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return nil, nil, errors.New("重置卡接口返回无效 JSON")
	}
	value, ok := payload["credits"]
	if !ok {
		return nil, nil, errors.New("重置卡列表缺失，无法判断到期时间")
	}
	var entries []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Type    string `json:"reset_type"`
		Granted string `json:"granted_at"`
		Expires string `json:"expires_at"`
	}
	if json.Unmarshal(value, &entries) != nil || string(value) == "null" || len(entries) > 10000 {
		return nil, nil, errors.New("重置卡列表格式无效")
	}
	credits := []resetCredit{}
	seen := map[string]bool{}
	for _, item := range entries {
		if item.Type != "codex_rate_limits" || item.Status != "available" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, item.Expires)
		if err != nil || item.ID == "" || len(item.ID) > 512 || seen[item.ID] {
			return nil, nil, errors.New("可用重置卡缺少有效 ID、到期时间或 ID 重复")
		}
		seen[item.ID] = true
		granted, _ := time.Parse(time.RFC3339Nano, item.Granted)
		credits = append(credits, resetCredit{ID: item.ID, Status: item.Status, GrantedAt: granted, ExpiresAt: at})
	}
	sort.Slice(credits, func(i, j int) bool {
		if credits[i].ExpiresAt.Equal(credits[j].ExpiresAt) {
			return credits[i].ID < credits[j].ID
		}
		return credits[i].ExpiresAt.Before(credits[j].ExpiresAt)
	})
	var applicable *int
	if field, ok := payload["applicable_available_count"]; ok && string(field) != "null" {
		var count int
		if json.Unmarshal(field, &count) != nil || count < 0 {
			return nil, nil, errors.New("重置卡适用数量无效")
		}
		applicable = &count
	}
	return credits, applicable, nil
}

func creditIDs(credits []resetCredit) []string {
	ids := make([]string, 0, len(credits))
	for _, c := range credits {
		ids = append(ids, c.ID)
	}
	return ids
}
func hasCredit(credits []resetCredit, id string) bool {
	for _, c := range credits {
		if c.ID == id {
			return true
		}
	}
	return false
}
func operationKey(identity, target string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + target))
	return hex.EncodeToString(sum[:])
}
func requestUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// refreshMu serializes refresh cycles; resetRunMu also serializes aliases of the
// same upstream account. Quota probes and model scheduling never wait on network
// calls while holding the engine policy lock.
func (e *engine) processReset(ctx context.Context, a account) {
	if a.Provider != "codex" || a.Disabled {
		return
	}
	e.resetRunMu.Lock()
	defer e.resetRunMu.Unlock()
	e.mu.Lock()
	cfg := e.cfg
	p := e.p.AutoReset
	timeout := e.p.TimeoutSeconds
	e.mu.Unlock()
	e.resetMu.Lock()
	s := e.resetSnapshots[a.ID]
	s.Enabled = resetEnabled(p, a)
	s.AttemptAt = time.Now()
	e.resetMu.Unlock()
	defer func() { e.resetMu.Lock(); e.resetSnapshots[a.ID] = s; e.resetMu.Unlock() }()
	api, err := newResetAPI(e.host, cfg, a)
	if err != nil {
		s.Error = err.Error()
		return
	}
	defer api.client.CloseIdleConnections()
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	credits, applicable, err := api.list(queryCtx)
	cancel()
	if err != nil {
		s.Error = err.Error()
		return
	}
	now := time.Now()
	s.Credits = credits
	s.CheckedAt = now
	s.Error = ""
	s.NextAt = time.Time{}
	s.Message = "暂无可用重置卡"
	for _, c := range credits {
		if c.ExpiresAt.After(now) {
			s.NextAt = c.ExpiresAt.Add(-time.Duration(p.BeforeHours * float64(time.Hour)))
			s.Message = "等待进入到期前自动使用窗口"
			break
		}
	}
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	if e.resetJournalError != "" {
		s.Error = e.resetJournalError
		return
	}
	// A successful list query must not erase a failed redemption, including after restart.
	s.LastResult = nil
	for _, previous := range e.resetJournal.Operations {
		if previous.Identity != api.identity {
			continue
		}
		if previous.LastResult != nil && (s.LastResult == nil || previous.LastResult.At.After(s.LastResult.At)) {
			copy := *previous.LastResult
			s.LastResult = &copy
		}
	}
	if s.LastResult != nil {
		s.Error = s.LastResult.Error
	}
	var key string
	var op resetOperation
	// Resume an uncertain transaction before considering any other credit.
	for k, old := range e.resetJournal.Operations {
		if old.Identity != api.identity {
			continue
		}
		if old.State == "success" && !old.CooldownCleared {
			s.Message = old.Message
			e.finishResetCooldown(a, k, old, &s)
			return
		}
		if old.State == "mismatch" && now.Before(old.ExpiresAt) {
			s.Error = "发现其他重置卡变为不可用；该账号暂停自动兑换至目标卡到期，请检查执行记录"
			return
		}
		if old.State == "pending" || old.State == "confirmed" {
			key = k
			op = old
			break
		}
	}
	if key != "" {
		if !op.ExpiresAt.After(now) {
			op.State = "expired"
			if op.LastResult == nil {
				op.LastResult = &resetResult{At: now, Error: "目标卡已到期，历史兑换结果未确认"}
			}
			s.LastResult, s.Error = op.LastResult, op.LastResult.Error
			op.Message = "目标卡已到期；停止重复提交"
			_ = e.commitReset(key, op, true)
			s.Message = op.Message
			return
		}
		missing := []string{}
		for _, id := range op.BeforeIDs {
			if !hasCredit(credits, id) {
				missing = append(missing, id)
			}
		}
		other := false
		for _, id := range missing {
			if id != op.Target {
				other = true
			}
		}
		if other {
			op.State = "mismatch"
			op.Message = "兑换后其他卡变为不可用，已暂停该账号，避免连续消耗"
			_ = e.commitReset(key, op, true)
			s.Error = op.Message
			return
		}
		if !hasCredit(credits, op.Target) {
			op.State = "success"
			op.LastResult = &resetResult{At: now}
			s.LastResult, s.Error = op.LastResult, ""
			op.Message = "目标重置卡已不可用，已核实并结束兑换，不再重复提交"
			if e.commitReset(key, op, true) != nil {
				s.Error = e.resetJournalError
				return
			}
			s.Message = op.Message
			e.finishResetCooldown(a, key, op, &s)
			return
		}
		if op.State == "confirmed" {
			s.Message = "上游已确认重置，等待卡片列表同步；不会再次兑换"
			e.finishResetCooldown(a, key, op, &s)
			return
		}
	} else {
		if !s.Enabled {
			s.Message = "此账号自动使用已关闭"
			return
		}
		if applicable != nil && *applicable == 0 {
			s.Message = "上游当前不允许使用重置卡，将继续检查"
			return
		}
		for _, credit := range credits {
			if !credit.ExpiresAt.After(now) || credit.ExpiresAt.Sub(now) > time.Duration(p.BeforeHours*float64(time.Hour)) {
				continue
			}
			k := operationKey(api.identity, credit.ID)
			old, exists := e.resetJournal.Operations[k]
			if exists && old.State != "deferred" {
				continue
			}
			key = k
			op = old
			if !exists {
				op = resetOperation{Account: a.Name, Identity: api.identity, Target: credit.ID, ExpiresAt: credit.ExpiresAt}
			}
			break
		}
		if key == "" {
			return
		}
	}
	if !s.Enabled {
		s.Message = "自动使用已关闭，未完成的兑换仍保留供下次核实"
		return
	}
	if now.Before(op.NextTryAt) {
		s.NextAt = op.NextTryAt
		s.Message = op.Message
		return
	}
	// A policy may have been disabled while the GET request was in flight.
	e.resetMu.Unlock()
	e.mu.Lock()
	currentPolicy := e.p.AutoReset
	enabled := resetEnabled(currentPolicy, a)
	e.mu.Unlock()
	e.resetMu.Lock()
	if !enabled {
		s.Enabled = false
		s.Message = "自动使用已关闭"
		return
	}
	if !op.ExpiresAt.After(time.Now()) {
		s.Message = "目标卡已到期；未发送兑换请求"
		return
	}
	if op.ExpiresAt.Sub(time.Now()) > time.Duration(currentPolicy.BeforeHours*float64(time.Hour)) {
		s.NextAt = op.ExpiresAt.Add(-time.Duration(currentPolicy.BeforeHours * float64(time.Hour)))
		s.Message = "提前使用时间已调整，等待新的使用窗口"
		return
	}
	if op.RequestID == "" || op.State == "deferred" {
		op.RequestID, err = requestUUID()
		if err != nil {
			s.Error = "无法生成兑换请求编号"
			return
		}
		op.BeforeIDs = creditIDs(credits)
	}
	op.State = "pending"
	op.Attempts++
	op.NextTryAt = now.Add(5 * time.Minute)
	op.Message = "正在兑换临期卡；请求编号已持久化"
	if e.commitReset(key, op, true) != nil {
		s.Error = e.resetJournalError
		return
	}
	// Release only the journal lock during network I/O. resetRunMu still prevents
	// simultaneous requests for account aliases and settings remain readable.
	e.resetMu.Unlock()
	consumeCtx, consumeCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	raw, httpStatus, consumeErr := api.call(consumeCtx, "POST", "/consume", map[string]string{"redeem_request_id": op.RequestID})
	consumeCancel()
	e.resetMu.Lock()
	result := api.result(raw, httpStatus, consumeErr)
	op.LastResult = &result
	s.LastResult, s.Error = &result, result.Error
	if consumeErr == nil && httpStatus >= 200 && httpStatus < 300 && (result.Code == "reset" || result.Code == "already_redeemed") && result.Error == "" {
		op.State = "confirmed"
		op.Message = "上游已确认重置，正在核实卡片消耗"
		op.NextTryAt = time.Time{}
		if e.commitReset(key, op, true) != nil {
			s.Error = e.resetJournalError
			return
		}
		e.finishResetCooldown(a, key, op, &s)
		s.Message = op.Message
		e.notifyRefresh() // Re-read quota before releasing quota-protection latches.
		return
	}
	// Only an explicit rejection proving no reset occurred permits a new UUID.
	// Nested error codes describe failures but do not prove an idempotent request can be replaced.
	var topLevel struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &topLevel)
	switch topLevel.Code {
	case "not_limited", "cooldown", "ineligible", "unavailable", "rate_limited", "no_available_credits":
		if consumeErr == nil && httpStatus >= 200 && httpStatus < 500 && httpStatus != 408 {
			op.State = "deferred"
			op.Message = "上游暂未执行重置（" + result.Code + "），5 分钟后重新检查"
		}
	}
	if op.State == "pending" {
		op.Message = "兑换结果未确认，先核实卡片状态；重试沿用原请求编号"
	}
	if e.commitReset(key, op, true) != nil {
		s.Error = e.resetJournalError
		return
	}
	s.Message = op.Message
	s.NextAt = op.NextTryAt
}

// resetMu is held. A confirmed reset clears only CPA's in-memory cooldown, never
// the auth file. Quota protection still needs fresh quota observations to recover.
func (e *engine) finishResetCooldown(a account, key string, op resetOperation, s *resetAccountStatus) {
	if op.CooldownCleared {
		return
	}
	e.resetMu.Unlock()
	var result struct {
		AuthIndex string `json:"auth_index"`
	}
	err := e.host("host.routing.reset_cooldown", map[string]string{"auth_index": a.AuthIndex}, &result)
	e.resetMu.Lock()
	if err != nil || result.AuthIndex != a.AuthIndex {
		s.Error = "重置已确认，但 CPA 冷却状态未清除；下次查询继续核实"
		return
	}
	op.CooldownCleared = true
	_ = e.commitReset(key, op, false)
}

// Detached copies keep JSON encoding race-free while background checks run.
func (e *engine) resetView(a account) (*resetAccountStatus, []resetEvent, string) {
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	var status *resetAccountStatus
	if a.Provider == "codex" {
		s := e.resetSnapshots[a.ID]
		s.Enabled = resetEnabled(e.p.AutoReset, a)
		s.Credits = append([]resetCredit{}, s.Credits...)
		status = &s
	}
	events := append([]resetEvent{}, e.resetJournal.Events...)
	return status, events, e.resetJournalError
}
