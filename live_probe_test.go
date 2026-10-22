package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in, read-only compatibility check. Never starts CPA, refreshes OAuth, or sends model requests.
// Account identifiers, credentials and response bodies are never logged.
func TestLiveQuotaCompatibility(t *testing.T) {
	dir := os.Getenv("QUOTA_GUARD_LIVE_AUTH_DIR")
	if dir == "" {
		t.Skip("set QUOTA_GUARD_LIVE_AUTH_DIR for the opt-in live check")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("cannot read live auth directory")
	}
	tested := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal("cannot read auth file")
		}
		var fields map[string]any
		if json.Unmarshal(raw, &fields) != nil {
			t.Fatal("invalid auth JSON")
		}
		provider := stringAt(fields, "type", "provider")
		if provider != "codex" && provider != "antigravity" {
			continue
		}
		if disabled, ok := fields["disabled"].(bool); ok && disabled {
			continue
		}
		host := func(method string, _ any, dest any) error {
			wrapped, _ := json.Marshal(map[string]json.RawMessage{"json": raw})
			return json.Unmarshal(wrapped, dest)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		buckets, probeErr := probeAccount(ctx, host, runtimeConfig{ProxyURL: os.Getenv("QUOTA_GUARD_LIVE_PROXY")}, account{Provider: provider})
		cancel()
		tested++
		if probeErr != nil {
			t.Errorf("live provider=%s: %v", provider, probeErr)
			continue
		}
		t.Logf("live provider=%s parsed_windows=%d", provider, len(buckets))
	}
	if tested == 0 {
		t.Fatal("no supported active accounts found")
	}
}
