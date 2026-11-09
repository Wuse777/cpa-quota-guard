package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clonePolicyForTest(p policy) policy {
	raw, _ := json.Marshal(p)
	var result policy
	_ = json.Unmarshal(raw, &result)
	return result
}

func TestUnrelatedPolicySavePreservesRecoveryLatches(t *testing.T) {
	e := testEngine(t)
	e.p.Rules = append(e.p.Rules, rule{Account: "b.json", Enabled: true, MinRemainingPercent: 20})
	putQuota(e, "a.json", 23)
	putQuota(e, "b.json", 23)
	_ = pickTwo(e)
	putQuota(e, "a.json", 26)
	putQuota(e, "b.json", 26)
	p := clonePolicyForTest(e.p)
	p.AutoReset.BeforeHours = 8
	if err := e.updatePolicy(p); err != nil {
		t.Fatal(err)
	}
	if !pickTwo(e).Reject {
		t.Fatal("saving reset settings must not release quota recovery latches")
	}
	p = clonePolicyForTest(e.p)
	p.Rules[1].MinRemainingPercent = 10
	if err := e.updatePolicy(p); err != nil {
		t.Fatal(err)
	}
	if got := pickTwo(e); got.AuthID != "b.json" {
		t.Fatalf("only the changed rule should be recalculated: %+v", got)
	}
	if !e.evaluate(e.snapshots["a.json"].Account, time.Now()).Protected {
		t.Fatal("another account lost its latch")
	}
}

func TestKnownDefaultWindowCannotSilentlyDisappear(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 0)
	s := e.snapshots["a.json"]
	s.Buckets = append(s.Buckets, bucket{ID: "secondary", RemainingPercent: 90})
	e.snapshots["a.json"] = s
	if !e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("precondition")
	}
	current, err := parseQuota("codex", []byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":10}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if e.checkWindowContinuity(s, current) == nil {
		t.Fatal("missing known primary must invalidate the new snapshot")
	}
	e.p.Rules[0].Windows = map[string]float64{"secondary": 20}
	if err = e.checkWindowContinuity(s, current); err != nil {
		t.Fatal("explicit windows must permit a deliberate plan change")
	}
}

func TestFullStopLineIsExplicitlyReported(t *testing.T) {
	e := testEngine(t)
	e.p.Rules[0].MinRemainingPercent = 97
	putQuota(e, "a.json", 100)
	if !e.evaluate(e.snapshots["a.json"].Account, time.Now()).Protected {
		t.Fatal("explicit 100% stop line blocks all usage")
	}
	if len(e.ruleWarnings(e.snapshots["a.json"].Account)) == 0 {
		t.Fatal("100% stop line needs an actionable warning")
	}
	raw, _ := json.Marshal(e.status())
	for _, field := range []string{`"health":"healthy"`, `"effective_rule":`, `"warnings":[`, `"build":`, `"version":"0.3.0"`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("missing status contract %s", field)
		}
	}
}

func TestSafeDefaultsAndLegacyPolicyMigration(t *testing.T) {
	dir := t.TempDir()
	p, err := loadPolicy(dir)
	if err != nil || p.AutoReset.Enabled || p.AutoReset.IncludeNewAccounts {
		t.Fatal("new installations must be opt-in")
	}
	raw := []byte(`{"revision":9,"mode":"enforce","auto_reset":{"enabled":true,"before_hours":6,"accounts":{}}}`)
	path := filepath.Join(dir, "policy.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err = loadPolicy(dir)
	if err != nil || !p.AutoReset.Enabled || !p.AutoReset.IncludeNewAccounts {
		t.Fatal("old explicit policy behavior must be preserved")
	}
	after, _ := os.ReadFile(path)
	if string(raw) != string(after) {
		t.Fatal("loading must not rewrite production policy")
	}
	a := account{Name: "new.json", Provider: "codex"}
	p.AutoReset.IncludeNewAccounts = false
	if resetEnabled(p.AutoReset, a) {
		t.Fatal("new account cannot inherit automatic redemption without opt-in")
	}
	p.AutoReset.Accounts[a.Name] = true
	if !resetEnabled(p.AutoReset, a) {
		t.Fatal("explicit account opt-in must override the default")
	}
}

func TestHistoricalRedemptionFailureIsNotCurrentSchedulingBlock(t *testing.T) {
	s := resetAccountStatus{Enabled: true, SchedulerState: "waiting", Message: "等待进入到期前自动使用窗口", Error: "历史兑换未确认", LastResult: &resetResult{At: time.Now().Add(-24 * time.Hour), Error: "历史兑换未确认"}}
	state, reason := resetSchedulerState(s, "")
	if state != "waiting" || reason != "" {
		t.Fatalf("history must not hide the current schedule: %s %s", state, reason)
	}
	s.QueryError = "当前卡片查询失败"
	state, reason = resetSchedulerState(s, "")
	if state != "error" || reason != s.QueryError {
		t.Fatal("current query failure must be distinct")
	}
}

func TestWebhookDeliveryDeduplicatesBacksOffAndRedactsURL(t *testing.T) {
	var count atomic.Int32
	var fail atomic.Bool
	var received string
	var receivedMu sync.Mutex
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		raw, _ := io.ReadAll(r.Body)
		receivedMu.Lock()
		received = string(raw)
		receivedMu.Unlock()
		if fail.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, "secret-response-body")
		}
	}))
	defer receiver.Close()
	e := testEngine(t)
	putQuota(e, "a.json", 10)
	s := e.snapshots["a.json"]
	s.CheckedAt = time.Now().Add(-time.Hour)
	e.snapshots["a.json"] = s
	path := filepath.Join(t.TempDir(), "webhook")
	endpoint := receiver.URL + "/private-webhook-secret?token=secret-query"
	if err := os.WriteFile(path, []byte(endpoint), 0600); err != nil {
		t.Fatal(err)
	}
	e.cfg.NotificationWebhookFile = path
	now := time.Now()
	e.dispatchNotifications(context.Background(), now)
	e.dispatchNotifications(context.Background(), now.Add(time.Second))
	if count.Load() != 1 {
		t.Fatal("duplicate issue should send only once within the suppression interval")
	}
	receivedMu.Lock()
	body := received
	receivedMu.Unlock()
	if !strings.Contains(body, "quota_stale") || strings.Contains(body, "a.json") || strings.Contains(body, "private-webhook-secret") {
		t.Fatalf("notification must contain a useful, anonymous event: %s", body)
	}
	fail.Store(true)
	e.dispatchNotifications(context.Background(), now.Add(time.Hour))
	e.dispatchNotifications(context.Background(), now.Add(time.Hour+time.Second))
	if count.Load() != 2 {
		t.Fatal("failed delivery should back off")
	}
	raw, _ := json.Marshal(e.status())
	for _, secret := range []string{endpoint, "private-webhook-secret", "secret-query", "secret-response-body"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("status leaked webhook data")
		}
	}
	if !strings.Contains(string(raw), "通知接收端 HTTP 500") {
		t.Fatal("bounded delivery error must be visible")
	}
}

func TestWebhookRejectsRedirectAndUnsafeFile(t *testing.T) {
	var targetCount atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCount.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	path := filepath.Join(t.TempDir(), "webhook")
	if err := os.WriteFile(path, []byte(redirect.URL), 0600); err != nil {
		t.Fatal(err)
	}
	if sendNotification(context.Background(), path, notificationEvent{}) == nil || targetCount.Load() != 0 {
		t.Fatal("webhook must not follow redirects")
	}
	if err := os.WriteFile(path, []byte("http://example.invalid/secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWebhook(path); err == nil {
		t.Fatal("remote cleartext webhooks must be rejected")
	}
}

func TestQuotaWorkerIsIndependentOfSlowCardQuery(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var quotaCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v8/management/requests/api-call" {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"status_code":200,"body":"{\"credits\":[]}"}`)
			return
		}
		quotaCalls.Add(1)
		fmt.Fprint(w, `{"rate_limit":{"primary_window":{"used_percent":10}}}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	_ = os.WriteFile(key, []byte("fixture-only"), 0600)
	host := func(method string, payload any, dest any) error {
		raw := `{"json":{"access_token":"fixture-token","account_id":"fixture-id"}}`
		if method == "host.auth.list" {
			raw = `{"files":[{"id":"a.json","name":"a.json","auth_index":"index","provider":"codex"}]}`
		}
		return json.Unmarshal([]byte(raw), dest)
	}
	e, err := newEngine(runtimeConfig{StateDir: dir, ManagementURL: server.URL, ManagementKeyFile: key, TestProbeBase: server.URL}, host)
	if err != nil {
		t.Fatal(err)
	}
	go e.run()
	defer func() { close(release); close(e.stop); <-e.done }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("card query did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for quotaCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	before := quotaCalls.Load()
	e.notifyRefresh()
	deadline = time.Now().Add(time.Second)
	for quotaCalls.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if quotaCalls.Load() <= before {
		t.Fatal("manual quota refresh waited for a blocked card query")
	}
}

func TestDifferentIdentitiesConsumeConcurrentlyAndStatusStaysResponsive(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method    string `json:"method"`
			AuthIndex string `json:"authIndex"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method == "POST" {
			started <- request.AuthIndex
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": `{"code":"reset"}`})
		} else {
			body, _ := json.Marshal(map[string]any{"credits": []map[string]any{{"id": "near", "status": "available", "reset_type": "codex_rate_limits", "expires_at": time.Now().Add(time.Hour)}}})
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": string(body)})
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	_ = os.WriteFile(key, []byte("fixture-only"), 0600)
	host := func(method string, payload any, dest any) error {
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"a","name":"a","auth_index":"a","provider":"codex"},{"id":"b","name":"b","auth_index":"b","provider":"codex"}]}`), dest)
		case "host.auth.get":
			id := payload.(map[string]string)["auth_index"]
			raw, _ := json.Marshal(map[string]any{"json": map[string]string{"account_id": id, "access_token": "fixture-token"}})
			return json.Unmarshal(raw, dest)
		case "host.routing.reset_cooldown":
			raw, _ := json.Marshal(payload)
			return json.Unmarshal(raw, dest)
		}
		return fmt.Errorf("unexpected host method")
	}
	e, err := newEngine(runtimeConfig{StateDir: dir, ManagementURL: server.URL, ManagementKeyFile: key}, host)
	if err != nil {
		t.Fatal(err)
	}
	e.p.AutoReset.Enabled = true
	e.p.AutoReset.IncludeNewAccounts = true
	done := make(chan error, 1)
	go func() { done <- e.refreshResets(context.Background(), true) }()
	defer func() { close(release); <-done }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("different upstream identities were serialized across a slow consume")
		}
	}
	responsive := make(chan struct{})
	go func() { _ = e.status(); close(responsive) }()
	select {
	case <-responsive:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("status waited for consume network I/O")
	}
}

func TestScheduledQuotaBackoffCanBeManuallyBypassed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) }))
	defer server.Close()
	host := func(method string, payload any, dest any) error {
		raw := `{"json":{"access_token":"fixture-token"}}`
		if method == "host.auth.list" {
			raw = `{"files":[{"id":"a","name":"a","auth_index":"a","provider":"codex"}]}`
		}
		return json.Unmarshal([]byte(raw), dest)
	}
	e, err := newEngine(runtimeConfig{StateDir: t.TempDir(), TestProbeBase: server.URL}, host)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.refreshQuota(context.Background(), false)
	_ = e.refreshQuota(context.Background(), false)
	if calls.Load() != 1 {
		t.Fatal("scheduled failure must back off")
	}
	_ = e.refreshQuota(context.Background(), true)
	if calls.Load() != 2 {
		t.Fatal("explicit manual refresh must bypass query backoff")
	}
}

func TestPendingAndExpiringRedemptionNotificationsHaveStableOperationIDs(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	key := operationKey("hashed-upstream-identity", "fixture-credit")
	e.resetJournal.Operations[key] = resetOperation{Identity: "hashed-upstream-identity", Target: "fixture-credit", State: "pending", Attempts: 2, ExpiresAt: now.Add(30 * time.Minute), LastResult: &resetResult{At: now, Error: "fixture failure"}}
	events := e.notificationEvents(now)
	kinds := map[string]bool{}
	for _, event := range events {
		kinds[event.Kind] = true
		if event.OperationID != key || event.AccountFingerprint == "" {
			t.Fatal("redemption notifications must be grouped without revealing account identity")
		}
	}
	if !kinds["redemption_pending"] || !kinds["redemption_expiring"] {
		t.Fatalf("missing actionable redemption notifications: %+v", events)
	}
	e.resetJournal.Operations[key] = resetOperation{State: "success", ExpiresAt: now.Add(time.Hour)}
	if len(e.notificationEvents(now)) != 0 {
		t.Fatal("successful redemptions must not keep raising failure notifications")
	}
}

func TestConfirmedRedemptionIsNotDowngradedToFailureAtExpiration(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "delayed-list"
	e.processReset(context.Background(), a)
	e.resetMu.Lock()
	for key, op := range e.resetJournal.Operations {
		op.ExpiresAt = time.Now().Add(-time.Minute)
		e.resetJournal.Operations[key] = op
	}
	e.resetMu.Unlock()
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("confirmed reset must never be consumed again")
	}
	for _, op := range e.resetJournal.Operations {
		if op.State != "success" || op.LastResult == nil || op.LastResult.Code != "reset" || op.LastResult.Error != "" {
			t.Fatalf("expiration must retain upstream success evidence: %+v", op)
		}
	}
}

func TestResetQueryBackoffCannotSleepThroughKnownRedemptionWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	p := resetPolicy{Enabled: true, BeforeHours: 0.1}
	previous := retryState{Failures: 8}
	credits := []resetCredit{{ID: "near", ExpiresAt: now.Add(7 * time.Minute)}}
	retry := resetQueryRetry(previous, credits, p, true, now)
	if retry.NextAt.After(now.Add(time.Minute)) {
		t.Fatal("retry must occur by the known six-minute redemption window")
	}
	credits[0].ExpiresAt = now.Add(5 * time.Minute)
	retry = resetQueryRetry(previous, credits, p, true, now)
	if retry.NextAt.After(now.Add(time.Minute)) {
		t.Fatal("query failures inside the window must retry at least every minute")
	}
	retry = resetQueryRetry(previous, credits, p, false, now)
	if retry.NextAt.Before(now.Add(10 * time.Minute)) {
		t.Fatal("a disabled account must retain ordinary backoff")
	}
	credits[0].ExpiresAt = now.Add(-time.Minute)
	retry = resetQueryRetry(previous, credits, p, true, now)
	if retry.NextAt.Before(now.Add(10 * time.Minute)) {
		t.Fatal("an expired card must not create a busy retry loop")
	}
}
