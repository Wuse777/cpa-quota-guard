package main

import (
	"context"
	"encoding/json"
	"errors"
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

func testEngine(t *testing.T) *engine {
	t.Helper()
	e, err := newEngine(runtimeConfig{StateDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.p.Mode = "enforce"
	e.p.Rules = []rule{{Account: "a.json", Enabled: true, MinRemainingPercent: 20}}
	return e
}
func putQuota(e *engine, id string, remaining float64) {
	e.snapshots[id] = snapshot{Account: account{ID: id, Name: id, Provider: "codex"}, CheckedAt: time.Now(), Buckets: []bucket{{ID: "primary", Label: "5 小时", RemainingPercent: remaining}}}
}
func pickTwo(e *engine) pickResponse {
	return e.pick(pickRequest{Provider: "codex", Model: "gpt-test", Candidates: []candidate{{ID: "a.json", Provider: "codex", Priority: 10}, {ID: "b.json", Provider: "codex", Priority: 0}}})
}

func TestThresholdSafetyRecoveryAndPriority(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 23)
	putQuota(e, "b.json", 80)
	if r := pickTwo(e); r.AuthID != "b.json" {
		t.Fatalf("must skip high priority protected account: %+v", r)
	}
	putQuota(e, "a.json", 26)
	if r := pickTwo(e); r.AuthID != "b.json" {
		t.Fatalf("must retain hysteresis: %+v", r)
	}
	putQuota(e, "a.json", 28)
	if r := pickTwo(e); r.AuthID != "a.json" {
		t.Fatalf("must recover at resume line: %+v", r)
	}
	putQuota(e, "a.json", 24)
	if r := pickTwo(e); r.AuthID != "a.json" {
		t.Fatalf("must remain usable above stop line: %+v", r)
	}
}

func TestAllProtectedRejectsRatherThanFallsBack(t *testing.T) {
	e := testEngine(t)
	e.p.Rules = append(e.p.Rules, rule{Account: "b.json", Enabled: true, MinRemainingPercent: 40})
	putQuota(e, "a.json", 10)
	putQuota(e, "b.json", 30)
	r := pickTwo(e)
	if !r.Handled || !r.Reject || r.RejectCode != "quota_guard_protected" {
		t.Fatalf("must explicitly reject: %+v", r)
	}
}

func TestEveryWindowLatchesEvenWhenAnotherWindowAlreadyBlocks(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 10)
	s := e.snapshots["a.json"]
	s.Buckets = append(s.Buckets, bucket{ID: "secondary", Label: "weekly", RemainingPercent: 10})
	e.snapshots["a.json"] = s
	if !e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("low windows must protect")
	}
	s.Buckets[0].RemainingPercent = 90
	s.Buckets[1].RemainingPercent = 26
	e.snapshots["a.json"] = s
	if !e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("second window must retain its own recovery latch")
	}
	s.Buckets[1].RemainingPercent = 28
	e.snapshots["a.json"] = s
	if e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("all windows recovered")
	}
}
func TestObserveAndDisabledRulesPreserveNativeSelection(t *testing.T) {
	e := testEngine(t)
	e.p.Mode = "observe"
	putQuota(e, "a.json", 0)
	if pickTwo(e).Handled {
		t.Fatal("observe must delegate")
	}
	e.p.Mode = "enforce"
	e.p.Rules[0].Enabled = false
	if pickTwo(e).Handled {
		t.Fatal("disabled rules must delegate")
	}
	e.p.Rules = nil
	if pickTwo(e).Handled {
		t.Fatal("unconfigured routes must delegate")
	}
}

func TestUnknownExpiredAndMissingWindows(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			e := testEngine(t)
			if allow {
				e.p.UnknownAction = "allow"
			}
			a := account{ID: "a.json", Name: "a.json", Provider: "codex"}
			if got := e.evaluate(a, time.Now()).Protected; got == allow {
				t.Fatal("wrong missing data policy")
			}
			putQuota(e, "a.json", 90)
			s := e.snapshots[a.ID]
			s.CheckedAt = time.Now().Add(-time.Hour)
			e.snapshots[a.ID] = s
			if got := e.evaluate(a, time.Now()).Protected; got == allow {
				t.Fatal("wrong stale data policy")
			}
			putQuota(e, "a.json", 90)
			e.p.Rules[0].Windows = map[string]float64{"secondary": 20}
			if got := e.evaluate(a, time.Now()).Protected; got == allow {
				t.Fatal("missing selected bucket must use unknown policy")
			}
		})
	}
}

func TestIndependentWindowThresholdAndAuxiliarySelection(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 80)
	s := e.snapshots["a.json"]
	s.Buckets = append(s.Buckets, bucket{ID: "code-review/primary", RemainingPercent: 0, Auxiliary: true})
	e.snapshots["a.json"] = s
	if e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("review must not block normal coding by default")
	}
	e.p.Rules[0].Windows = map[string]float64{"code-review/primary": 0}
	if !e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("explicit zero threshold must protect exhausted auxiliary bucket")
	}
	e.p.Rules[0].Windows = map[string]float64{"primary": 90}
	if !e.evaluate(s.Account, time.Now()).Protected {
		t.Fatal("per-window threshold must override account threshold")
	}
}

func TestRoundRobinShrinkingCandidatePool(t *testing.T) {
	e := testEngine(t)
	e.p.Rules = append(e.p.Rules, rule{Account: "b.json", Enabled: true, MinRemainingPercent: 20})
	putQuota(e, "a.json", 80)
	putQuota(e, "b.json", 80)
	req := pickRequest{Candidates: []candidate{{ID: "a.json", Provider: "codex"}, {ID: "b.json", Provider: "codex"}, {ID: "c.json", Provider: "codex"}}}
	for _, want := range []string{"a.json", "b.json", "c.json", "a.json"} {
		if got := e.pick(req).AuthID; got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
	req.Candidates = req.Candidates[1:]
	if got := e.pick(req).AuthID; got != "b.json" {
		t.Fatal(got)
	}
}

func TestPolicyDurabilityConflictAndValidation(t *testing.T) {
	e := testEngine(t)
	p := e.p
	p.Revision = 0
	if !errors.Is(e.updatePolicy(p), errRevision) {
		t.Fatal("stale writes must conflict")
	}
	p = e.p
	p.Mode = "observe"
	if err := e.updatePolicy(p); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPolicy(e.cfg.StateDir)
	if err != nil || saved.Mode != "observe" || saved.Revision != 2 {
		t.Fatalf("persist %+v %v", saved, err)
	}
	if st, err := os.Stat(filepath.Join(e.cfg.StateDir, "policy.json")); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("policy must be private")
	}
	bad := saved
	bad.PollSeconds = 0
	if validatePolicy(bad) == nil {
		t.Fatal("must validate intervals")
	}
	bad = saved
	bad.Rules = append(bad.Rules, bad.Rules[0])
	if validatePolicy(bad) == nil {
		t.Fatal("must reject duplicate rules")
	}
	if decodeJSON([]byte(`{} {}`), &policy{}) == nil {
		t.Fatal("must reject trailing JSON")
	}
	bad = saved
	bad.TimeoutSeconds = 99
	if e.updatePolicy(bad) == nil {
		t.Fatal("invalid policy must not publish")
	}
}

func TestPolicyWriteFailureKeepsCurrentState(t *testing.T) {
	e := testEngine(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	e.cfg.StateDir = blocker
	p := e.p
	p.Mode = "observe"
	if e.updatePolicy(p) == nil || e.p.Mode != "enforce" {
		t.Fatal("failed persistence must not alter active policy")
	}
}

func TestConcurrentPickStatusAndPolicyUpdate(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 10)
	putQuota(e, "b.json", 90)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				_ = pickTwo(e)
				_ = e.status()
			}
		}()
	}
	for n := 0; n < 10; n++ {
		e.mu.Lock()
		p := e.p
		e.mu.Unlock()
		if err := e.updatePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestRefreshFetchesHostAuthAndHandlesFailureWithoutFalseRecovery(t *testing.T) {
	low := true
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-test" {
			t.Error("missing auth")
		}
		used := 10
		if low {
			used = 90
		}
		fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":%d}}}`, used)
	}))
	defer server.Close()
	e := testEngine(t)
	e.cfg.TestProbeBase = server.URL
	e.host = func(method string, payload any, dest any) error {
		var raw string
		switch method {
		case "host.auth.list":
			raw = `{"files":[{"id":"a.json","name":"a.json","auth_index":"a","provider":"codex"}]}`
		case "host.auth.get":
			raw = `{"json":{"access_token":"secret-test"}}`
		default:
			return errors.New("unexpected callback")
		}
		return json.Unmarshal([]byte(raw), dest)
	}
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.evaluate(e.snapshots["a.json"].Account, time.Now()).Protected {
		t.Fatal("low quota must protect")
	}
	fail = true
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.evaluate(e.snapshots["a.json"].Account, time.Now()).Protected {
		t.Fatal("failed query must not reset quota")
	}
	fail = false
	low = false
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.evaluate(e.snapshots["a.json"].Account, time.Now()).Protected {
		t.Fatal("successful query must recover")
	}
	s, _ := json.Marshal(e.status())
	if strings.Contains(string(s), "secret-test") {
		t.Fatal("status must not contain token")
	}
}

func TestPublicResourceDoesNotExposeAccountsOrAcceptWrites(t *testing.T) {
	e := testEngine(t)
	putQuota(e, "a.json", 20)
	r := e.management(managementRequest{Method: "GET", Path: "/v0/resource/plugins/quota-guard/index"})
	if r.StatusCode != 200 || strings.Contains(string(r.Body), "a.json") {
		t.Fatal("resource must be static")
	}
	r = e.management(managementRequest{Method: "PUT", Path: "/v0/resource/plugins/quota-guard/index", Body: []byte(`{}`)})
	if r.StatusCode != 404 {
		t.Fatal("resource must not mutate policy")
	}
}
