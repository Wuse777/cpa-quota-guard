package main

import (
	"testing"
	"time"
)

func TestQuotaParsers(t *testing.T) {
	tests := []struct {
		name, provider, raw string
		count               int
		first               string
		percent             float64
	}{
		{"codex windows", "codex", `{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000},"secondary_window":{"used_percent":40}}}`, 2, "primary", 20},
		{"codex aliases", "codex", `{"rateLimit":{"primaryWindow":{"usedPercent":"0"}}}`, 1, "primary", 100},
		{"codex exhausted", "codex", `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":40}}}`, 1, "primary", 0},
		{"antigravity groups", "antigravity", `{"groups":[{"displayName":"Claude","buckets":[{"window":"WEEKLY","remainingFraction":0.25,"resetTime":"2026-10-23T00:00:00Z"}]}]}`, 1, "Claude/WEEKLY", 25},
		{"antigravity legacy", "antigravity", `{"models":{"claude-sonnet":{"quotaInfo":{"remainingFraction":0}}}}`, 1, "claude-sonnet", 0},
		{"antigravity snake", "antigravity", `{"groups":[{"display_name":"Gemini","buckets":[{"bucket_id":"b1","remaining_fraction":"1"}]}]}`, 1, "Gemini/b1", 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseQuota(tt.provider, []byte(tt.raw), time.Now())
			if err != nil || len(b) != tt.count || b[0].ID != tt.first || b[0].RemainingPercent != tt.percent {
				t.Fatalf("got %+v err %v", b, err)
			}
		})
	}
}
func TestMalformedQuotaNeverBecomesAvailable(t *testing.T) {
	for _, raw := range []string{`{}`, `{"rate_limit":{"primary_window":{}}}`, `{"rate_limit":{"primary_window":{"used_percent":-1}}}`, `{"rate_limit":{"primary_window":{"used_percent":101}}}`, `<html>`, `{"rate_limit":{"primary_window":{"used_percent":"NaN"}}}`} {
		if _, err := parseQuota("codex", []byte(raw), time.Now()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parseQuota("antigravity", []byte(`{"groups":[{"displayName":"x","buckets":[{"window":"x","remainingFraction":"NaN"}]}]}`), time.Now()); err == nil {
		t.Fatal("NaN accepted")
	}
}
func TestLoopbackTestEndpointRestriction(t *testing.T) {
	for _, s := range []string{"http://example.com", "https://127.0.0.1", "http://127.0.0.1/steal", "http://127.0.0.1?x=y", "http://user:pass@127.0.0.1"} {
		if validateRuntime(runtimeConfig{TestProbeBase: s}) == nil {
			t.Fatal(s)
		}
	}
	if err := validateRuntime(runtimeConfig{TestProbeBase: "http://127.0.0.1:12345"}); err != nil {
		t.Fatal(err)
	}
}

func TestPartialMalformedQuotaDoesNotSilentlyDropAWindow(t *testing.T) {
	tests := []struct{ provider, raw string }{
		{"codex", `{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{}}}`},
		{"antigravity", `{"groups":[{"displayName":"Claude","buckets":[{"window":"DAILY","remainingFraction":1},{"window":"WEEKLY"}]}]}`},
	}
	for _, tt := range tests {
		if _, err := parseQuota(tt.provider, []byte(tt.raw), time.Now()); err == nil {
			t.Fatalf("accepted incomplete %s quota", tt.provider)
		}
	}
}
