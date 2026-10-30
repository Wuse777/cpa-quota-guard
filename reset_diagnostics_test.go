package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedemptionFailuresSurviveListRefreshRestartAndExpiration(t *testing.T) {
	for _, mode := range []string{"nested-error", "bad-json", "unknown-code", "ambiguous", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			e, f, a := resetTestEngine(t)
			f.mode = mode
			e.processReset(context.Background(), a)
			first := e.resetSnapshots[a.ID]
			if first.Error == "" || first.LastResult == nil || first.LastResult.ManagementStatus != 200 || first.LastResult.HTTPStatus == 0 {
				t.Fatalf("missing persisted diagnostics: %#v", first)
			}
			if mode == "nested-error" && (first.LastResult.HTTPStatus != 403 || first.LastResult.Code != "policy_denied") {
				t.Fatalf("missing nested upstream failure: %#v", first.LastResult)
			}
			e.processReset(context.Background(), a)
			if e.resetSnapshots[a.ID].Error != first.Error || resetRequests(f) != 1 {
				t.Fatal("list refresh erased error or bypassed retry backoff")
			}
			raw, err := os.ReadFile(filepath.Join(e.cfg.StateDir, "reset-journal.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"fake-token", "management-fixture-secret", "other-secret", "<html>"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("journal leaked secret or raw body: %s", secret)
				}
			}
			e2, err := newEngine(e.cfg, e.host)
			if err != nil {
				t.Fatal(err)
			}
			e2.processReset(context.Background(), a)
			if e2.resetSnapshots[a.ID].Error != first.Error {
				t.Fatal("restart erased redemption error")
			}
			e2.resetMu.Lock()
			for k, op := range e2.resetJournal.Operations {
				op.ExpiresAt = time.Now().Add(-time.Minute)
				e2.resetJournal.Operations[k] = op
			}
			e2.resetMu.Unlock()
			f.mu.Lock()
			f.credits = f.credits[1:]
			f.mu.Unlock()
			e2.processReset(context.Background(), a)
			e2.processReset(context.Background(), a)
			if e2.resetSnapshots[a.ID].Error != first.Error {
				t.Fatal("expiration or future card hid prior failure")
			}
		})
	}
}

func TestSuccessClearsStickyFailureWithoutLosingHistory(t *testing.T) {
	e, f, a := resetTestEngine(t)
	f.mode = "nested-error"
	e.processReset(context.Background(), a)
	f.mode = ""
	dueRetry(e)
	e.processReset(context.Background(), a)
	if e.resetSnapshots[a.ID].Error != "" {
		t.Fatal("confirmed reset must clear current error")
	}
	e.processReset(context.Background(), a)
	if e.resetSnapshots[a.ID].Error != "" {
		t.Fatal("reconciled reset restored stale failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[0] != f.requests[1] {
		t.Fatal("uncertain failure must keep its UUID")
	}
	found := false
	for _, event := range e.resetJournal.Events {
		if event.Result != nil && event.Result.HTTPStatus == 403 {
			found = true
		}
	}
	if !found {
		t.Fatal("success removed historical failure details")
	}
}

func TestCPAManagementFailuresAreDistinctFromUpstreamFailures(t *testing.T) {
	for _, mode := range []string{"forbidden", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			e, f, a := resetTestEngine(t)
			f.managementMode = mode
			e.processReset(context.Background(), a)
			result := e.resetSnapshots[a.ID].LastResult
			if result == nil || result.HTTPStatus != 0 || result.Error == "" || result.ManagementStatus == 0 || resetRequests(f) != 0 {
				t.Fatalf("misreported CPA failure: %#v", result)
			}
			e2, err := newEngine(e.cfg, e.host)
			if err != nil {
				t.Fatal(err)
			}
			e2.processReset(context.Background(), a)
			if e2.resetSnapshots[a.ID].Error != result.Error {
				t.Fatal("CPA forwarding failure was not durable")
			}
		})
	}
}

func TestResetManagementKeyUnavailableNeverFallsBackToDirectConsume(t *testing.T) {
	e, f, a := resetTestEngine(t)
	if err := os.Remove(e.cfg.ManagementKeyFile); err != nil {
		t.Fatal(err)
	}
	e.processReset(context.Background(), a)
	if e.resetSnapshots[a.ID].Error == "" || resetRequests(f) != 0 {
		t.Fatal("missing key must stop with actionable failure")
	}
}

func TestManagementURLRejectsRemoteTargetsAndRedirects(t *testing.T) {
	for _, value := range []string{"https://127.0.0.1:8317", "http://example.invalid", "http://127.0.0.1:8317/path", "http://user:password@127.0.0.1:8317"} {
		if validateRuntime(runtimeConfig{ManagementURL: value}) == nil {
			t.Fatalf("unsafe management address accepted: %s", value)
		}
	}
	e, f, a := resetTestEngine(t)
	api, err := newResetAPI(e.host, e.cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	defer api.client.CloseIdleConnections()
	if api.client.CheckRedirect(nil, nil) == nil {
		t.Fatal("management key could follow a redirect")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.managementCalls) != 0 {
		t.Fatal("constructing adapter unexpectedly performed a request")
	}
}
