package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type resetFixture struct {
	mu           sync.Mutex
	credits      []resetCredit
	requests     []string
	consumed     map[string]bool
	mode         string
	cooldowns    int
	failCooldown bool
}

func resetTestEngine(t *testing.T) (*engine, *resetFixture, account) {
	t.Helper()
	f := &resetFixture{credits: []resetCredit{
		{ID: "near", Status: "available", ExpiresAt: time.Now().Add(5 * time.Hour)},
		{ID: "far", Status: "available", ExpiresAt: time.Now().Add(8 * 24 * time.Hour)},
	}, consumed: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			t.Error("missing account auth")
		}
		if r.Header.Get("Chatgpt-Account-Id") != "upstream-account" {
			t.Error("missing account id")
		}
		if r.Method == "GET" {
			if f.mode == "bad-list" {
				fmt.Fprint(w, `{"available_count":2}`)
				return
			}
			list := []map[string]any{}
			for _, c := range f.credits {
				list = append(list, map[string]any{"id": c.ID, "status": c.Status, "reset_type": "codex_rate_limits", "expires_at": c.ExpiresAt})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"credits": list, "available_count": len(list)})
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := req["redeem_request_id"]
		f.requests = append(f.requests, id)
		if f.mode == "deferred" {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"code":"not_limited"}`)
			return
		}
		if f.mode == "ambiguous" {
			w.WriteHeader(502)
			fmt.Fprint(w, `{}`)
			return
		}
		if f.consumed[id] {
			fmt.Fprint(w, `{"code":"already_redeemed"}`)
			return
		}
		f.consumed[id] = true
		remove := 0
		if f.mode == "wrong-card" {
			remove = 1
		}
		if f.mode != "delayed-list" {
			f.credits = append(f.credits[:remove], f.credits[remove+1:]...)
		}
		if f.mode == "lost-response" {
			w.WriteHeader(502)
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"code":"reset"}`)
	}))
	t.Cleanup(server.Close)
	host := func(method string, payload, dest any) error {
		raw := `{"json":{"access_token":"fake-token","account_id":"upstream-account"}}`
		if method == "host.routing.reset_cooldown" {
			f.mu.Lock()
			f.cooldowns++
			fail := f.failCooldown
			f.mu.Unlock()
			if fail {
				return fmt.Errorf("mock cooldown failure")
			}
			raw = `{"auth_index":"index"}`
		}
		return json.Unmarshal([]byte(raw), dest)
	}
	e, err := newEngine(runtimeConfig{StateDir: t.TempDir(), TestProbeBase: server.URL}, host)
	if err != nil {
		t.Fatal(err)
	}
	return e, f, account{ID: "a", Name: "a.json", AuthIndex: "index", Provider: "codex"}
}
func resetRequests(f *resetFixture) int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.requests) }
func dueRetry(e *engine) {
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	for k, op := range e.resetJournal.Operations {
		op.NextTryAt = time.Time{}
		e.resetJournal.Operations[k] = op
	}
}

func TestAutomaticResetAtDeadlineAndDurableSingleConsumption(t *testing.T) {
	e, f, a := resetTestEngine(t)
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("must consume the near-expiry card")
	}
	e.processReset(context.Background(), a)
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("must not consume a far-future card or repeat successful reset")
	}
	e2, err := newEngine(e.cfg, e.host)
	if err != nil {
		t.Fatal(err)
	}
	e2.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("restart must retain consumed state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cooldowns != 1 {
		t.Fatalf("clear cooldown once after confirmation: %d", f.cooldowns)
	}
	st, err := os.Stat(filepath.Join(e.cfg.StateDir, "reset-journal.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("journal must be private")
	}
	raw, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "reset-journal.json"))
	if strings.Contains(string(raw), "fake-token") {
		t.Fatal("journal contains token")
	}
}
func TestResetWaitsUntilSixHoursAndHonorsSwitches(t *testing.T) {
	for _, mode := range []string{"future", "master-off", "account-off", "disabled", "other-provider", "expired"} {
		t.Run(mode, func(t *testing.T) {
			e, f, a := resetTestEngine(t)
			switch mode {
			case "future":
				f.credits[0].ExpiresAt = time.Now().Add(6*time.Hour + time.Minute)
			case "master-off":
				e.p.AutoReset.Enabled = false
			case "account-off":
				e.p.AutoReset.Accounts[a.Name] = false
			case "disabled":
				a.Disabled = true
			case "other-provider":
				a.Provider = "antigravity"
			case "expired":
				f.credits[0].ExpiresAt = time.Now().Add(-time.Second)
			}
			e.processReset(context.Background(), a)
			if resetRequests(f) != 0 {
				t.Fatal("must not consume")
			}
		})
	}
}
func TestAmbiguousResponseRetriesSameRequestAfterRestart(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "ambiguous"
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("first attempt")
	}
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("retry must back off")
	}
	e2, err := newEngine(e.cfg, e.host)
	if err != nil {
		t.Fatal(err)
	}
	dueRetry(e2)
	e2.processReset(context.Background(), a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[0] != f.requests[1] {
		t.Fatal("uncertain retry must reuse persisted UUID")
	}
}
func TestLostSuccessfulResponseReconcilesWithoutAnotherPOST(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "lost-response"
	e.processReset(context.Background(), a)
	e2, err := newEngine(e.cfg, e.host)
	if err != nil {
		t.Fatal(err)
	}
	e2.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("missing card proves no further consume needed")
	}
	for _, op := range e2.resetJournal.Operations {
		if op.State != "success" {
			t.Fatal(op.State)
		}
	}
}
func TestConfirmedResetWithDelayedListNeverPostsAgain(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "delayed-list"
	e.processReset(context.Background(), a)
	dueRetry(e)
	for i := 0; i < 3; i++ {
		e.processReset(context.Background(), a)
	}
	if resetRequests(f) != 1 {
		t.Fatal("confirmed means GET verification only")
	}
}
func TestCooldownFailureRetriesCallbackWithoutRedeemingAgain(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.failCooldown = true
	e.processReset(context.Background(), a)
	if !strings.Contains(e.resetSnapshots[a.ID].Error, "冷却") {
		t.Fatal("callback failure must be visible")
	}
	e.processReset(context.Background(), a)
	f.failCooldown = false
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 || f.cooldowns != 3 {
		t.Fatal("retry callback only, never consume again")
	}
	for _, op := range e.resetJournal.Operations {
		if !op.CooldownCleared {
			t.Fatal("cooldown completion must be durable")
		}
	}
}
func TestWrongCardConsumptionPausesAccount(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "wrong-card"
	e.processReset(context.Background(), a)
	e.processReset(context.Background(), a)
	e.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("must stop after upstream consumes a different card")
	}
	if !strings.Contains(e.resetSnapshots[a.ID].Error, "暂停") {
		t.Fatal("must surface mismatch")
	}
}
func TestDefiniteRejectionCanRetryWithNewRequestID(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "deferred"
	e.processReset(context.Background(), a)
	dueRetry(e)
	e.processReset(context.Background(), a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[0] == f.requests[1] {
		t.Fatal("explicit no-reset rejection can start a fresh attempt")
	}
}
func TestDuplicateAuthAliasesDoNotConsumeTwice(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "delayed-list"
	b := a
	b.ID = "alias"
	b.Name = "alias.json"
	var wg sync.WaitGroup
	for _, ac := range []account{a, b} {
		wg.Add(1)
		go func(ac account) { defer wg.Done(); e.processReset(context.Background(), ac) }(ac)
	}
	wg.Wait()
	if resetRequests(f) != 1 {
		t.Fatal("aliases of upstream account must share journal identity")
	}
}
func TestResetFailsClosedOnJournalFailureAndMalformedList(t *testing.T) {
	for _, mode := range []string{"write-failure", "bad-list", "bad-journal"} {
		t.Run(mode, func(t *testing.T) {
			e, f, a := resetTestEngine(t)
			switch mode {
			case "write-failure":
				p := filepath.Join(t.TempDir(), "blocker")
				_ = os.WriteFile(p, []byte("x"), 0600)
				e.cfg.StateDir = p
			case "bad-list":
				f.mode = mode
			case "bad-journal":
				_ = os.WriteFile(filepath.Join(e.cfg.StateDir, "reset-journal.json"), []byte("broken"), 0600)
				var err error
				e, err = newEngine(e.cfg, e.host)
				if err != nil {
					t.Fatal("quota engine must still load")
				}
			}
			e.processReset(context.Background(), a)
			if resetRequests(f) != 0 {
				t.Fatal("must not POST if durable tracking or card metadata fails")
			}
			if e.resetSnapshots[a.ID].Error == "" {
				t.Fatal("must explain failure")
			}
		})
	}
}
func TestResetParserAndOldPolicyMigration(t *testing.T) {
	for _, raw := range []string{`{}`, `{"credits":null}`, `{"credits":[{"status":"available","reset_type":"codex_rate_limits","id":"x","expires_at":"bad"}]}`} {
		if _, _, err := parseResetCredits([]byte(raw)); err == nil {
			t.Fatal("bad metadata accepted")
		}
	}
	p := defaultPolicy()
	p.AutoReset.BeforeHours = 0
	if validatePolicy(p) == nil {
		t.Fatal("invalid lead hours")
	}
	old := defaultPolicy()
	raw, _ := json.Marshal(old)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "auto_reset")
	raw, _ = json.Marshal(fields)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "policy.json"), raw, 0600)
	migrated, err := loadPolicy(dir)
	if err != nil || !migrated.AutoReset.Enabled || migrated.AutoReset.BeforeHours != 6 {
		t.Fatal("old policy should inherit authorized six-hour automatic reset")
	}
}

func TestReconfigureReadsJournalAfterRetiringWorkerFinishes(t *testing.T) {
	old, f, a := resetTestEngine(t)
	f.mode = "delayed-list"
	previousHost := host
	active.Lock()
	previous := active.e
	active.e = old
	active.Unlock()
	host = old.host
	defer func() {
		active.Lock()
		current := active.e
		active.e = previous
		active.Unlock()
		close(current.stop)
		<-current.done
		host = previousHost
	}()
	go func() {
		<-old.stop
		old.processReset(context.Background(), a)
		close(old.done)
	}()
	config := fmt.Sprintf("state_dir: %s\ntest_probe_base: %s\n", old.cfg.StateDir, old.cfg.TestProbeBase)
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
	if err := configure(raw); err != nil {
		t.Fatal(err)
	}
	active.RLock()
	current := active.e
	active.RUnlock()
	current.processReset(context.Background(), a)
	if resetRequests(f) != 1 {
		t.Fatal("hot reconfigure lost the retiring worker's transaction")
	}
}
