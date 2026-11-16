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

type activationModelFixture struct {
	mu         sync.Mutex
	registered []string
	catalog    string
	response   string
	status     int
	management int
	posts      int
	request    struct {
		AuthIndex string            `json:"authIndex"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data"`
		Proxy     string            `json:"proxy_url"`
	}
	started chan struct{}
	cancel  bool
}

const activationFixtureCatalog = `{"models":[
	{"id":"gpt-6-astra","type":"openai","thinking":{"levels":["high","medium","low"]}},
	{"id":"gpt-6-luna","type":"openai","thinking":{"levels":["high","medium","low"]}},
	{"id":"gpt-5.6-luna","type":"openai","thinking":{"levels":["medium","low"]},"config":{"override_header":{"user-agent":"codex-tui/0.154.0 (Mac OS fixture)","originator":"codex-tui","Authorization":"Bearer forbidden-static-secret","Cookie":"forbidden-cookie"}}},
	{"id":"gpt-5-mini","type":"openai","thinking":{"levels":["high","minimal","low"]}},
	{"id":"gpt-5-nano","type":"openai","thinking":{"levels":["medium","none","low"]}},
	{"id":"gpt-5-spark","type":"openai","thinking":{"levels":["low"]}},
	{"id":"codex-auto-review","type":"openai","thinking":{"levels":["low"]}},
	{"id":"gpt-image-2","type":"openai"},
	{"id":"gpt-unknown-effort","type":"openai","thinking":{"levels":["future"]}}
]}`

func activationTestModel(t *testing.T) (runtimeConfig, hostCall, account, *activationModelFixture) {
	t.Helper()
	f := &activationModelFixture{
		registered: []string{"gpt-6-astra", "gpt-6-luna", "gpt-5.6-luna", "gpt-5-spark", "codex-auto-review", "gpt-image-2"},
		catalog:    activationFixtureCatalog, status: 200,
		response: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n",
	}
	a := account{ID: "auth-id", AuthIndex: "auth-index", Name: "fake@example.com.json", Provider: "codex"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer activation-management-secret" {
			t.Error("missing management authentication")
		}
		f.mu.Lock()
		switch r.URL.Path {
		case "/v8/management/credentials/models":
			if r.Method != "GET" || r.URL.Query().Get("name") != a.ID {
				t.Error("model discovery must use the target credential ID")
			}
			models := make([]map[string]string, 0, len(f.registered))
			for _, id := range f.registered {
				models = append(models, map[string]string{"id": id})
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/v8/management/routing/model-definitions/codex":
			catalog := f.catalog
			f.mu.Unlock()
			fmt.Fprint(w, catalog)
		case "/v8/management/requests/api-call":
			f.posts++
			if err := json.NewDecoder(r.Body).Decode(&f.request); err != nil {
				t.Error(err)
			}
			status, response, management, cancel, started := f.status, f.response, f.management, f.cancel, f.started
			f.mu.Unlock()
			if started != nil {
				close(started)
			}
			if cancel {
				<-r.Context().Done()
				return
			}
			if management != 0 {
				w.WriteHeader(management)
				fmt.Fprint(w, `{"error":"Bearer activation-management-secret; fake-access-token"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": response})
		default:
			f.mu.Unlock()
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	state := t.TempDir()
	keyFile := filepath.Join(state, "management.key")
	if err := os.WriteFile(keyFile, []byte("activation-management-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig{StateDir: state, ManagementURL: server.URL, ManagementKeyFile: keyFile, ProxyURL: "http://127.0.0.1:7999"}
	host := func(method string, payload, dest any) error {
		if method != "host.auth.get" {
			t.Fatalf("activation must not invoke model callbacks or retry routes: %s", method)
		}
		rawPayload, _ := json.Marshal(payload)
		if !strings.Contains(string(rawPayload), a.AuthIndex) {
			t.Fatal("wrong account credentials requested")
		}
		raw, _ := json.Marshal(map[string]any{"json": json.RawMessage(`{"access_token":"fake-access-token","account_id":"chatgpt-target","proxy_url":"socks5://127.0.0.1:7888"}`)})
		return json.Unmarshal(raw, dest)
	}
	return cfg, host, a, f
}

func TestActivationModelsSelectRegisteredNormalPoolAndLowestEffort(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	f.registered = append(f.registered, "gpt-5-nano", "gpt-5-mini", "gpt-unknown-effort", "unknown-alias")
	models, err := activationModels(host, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 5 || models[0].ID != "gpt-5-nano" || models[0].LowestEffort != "none" || models[1].ID != "gpt-5-mini" || models[1].LowestEffort != "minimal" {
		t.Fatalf("unexpected normal models/minimum efforts: %#v", models)
	}
	for _, m := range models {
		if strings.Contains(m.ID, "spark") || strings.Contains(m.ID, "review") || strings.Contains(m.ID, "image") || strings.Contains(m.ID, "unknown") {
			t.Fatalf("unverified quota model exposed: %#v", m)
		}
	}
}

func TestExecuteActivationUsesOneFixedAccountMinimalRequest(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	result, err := executeActivation(context.Background(), host, cfg, a, "")
	if err != nil || !result.Confirmed || result.Ambiguous || result.Model != "gpt-5.6-luna" || result.Reasoning != "low" || result.InputTokens != 8 || result.OutputTokens != 1 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.request.AuthIndex != a.AuthIndex || f.request.Method != "POST" || f.request.URL != activationResponsesURL || f.request.Proxy != "socks5://127.0.0.1:7888" {
		t.Fatalf("unexpected forwarding request: %#v posts=%d", f.request, f.posts)
	}
	if f.request.Header["Authorization"] != "Bearer $TOKEN$" || f.request.Header["Chatgpt-Account-Id"] != "chatgpt-target" || f.request.Header["Accept"] != "text/event-stream" {
		t.Fatal("missing account-bound headers")
	}
	if f.request.Header["User-Agent"] != "codex-tui/0.154.0 (Mac OS fixture)" || f.request.Header["Cookie"] != "" || f.request.Header["Session_id"] == "" {
		t.Fatal("model-required headers missing or unsafe catalog header copied")
	}
	var body map[string]any
	if json.Unmarshal([]byte(f.request.Data), &body) != nil || body["store"] != false || body["stream"] != true || body["model"] != result.Model {
		t.Fatalf("unexpected activation payload: %s", f.request.Data)
	}
	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "tools", "previous_response_id", "service_tier", "temperature"} {
		if _, ok := body[field]; ok {
			t.Errorf("unsupported, expensive, or stateful parameter %s included", field)
		}
	}
	if strings.Contains(f.request.Data, "fake-access-token") || strings.Contains(f.request.Data, "activation-management-secret") {
		t.Fatal("credentials in model body")
	}
}

func TestActivationPreferredModelUnavailableDoesNotFallBackOrSend(t *testing.T) {
	for _, preferred := range []string{"gpt-not-registered", "gpt-5-spark", "codex-auto-review", "gpt-image-2", "gpt-6-luna(high)"} {
		t.Run(preferred, func(t *testing.T) {
			cfg, host, a, f := activationTestModel(t)
			result, err := executeActivation(context.Background(), host, cfg, a, preferred)
			if err == nil || result.Ambiguous || result.Confirmed || f.posts != 0 {
				t.Fatalf("model failure must not send/fallback: result=%#v err=%v posts=%d", result, err, f.posts)
			}
			var uncertain interface{ ActivationUncertain() bool }
			if !errors.As(err, &uncertain) || uncertain.ActivationUncertain() {
				t.Fatal("unavailable model incorrectly marked sent")
			}
		})
	}
}

func TestActivationPreferredModelUsesItsMinimum(t *testing.T) {
	cfg, host, a, _ := activationTestModel(t)
	result, err := executeActivation(context.Background(), host, cfg, a, "gpt-6-astra")
	if err != nil || result.Model != "gpt-6-astra" || result.Reasoning != "low" {
		t.Fatalf("preferred model effort: %#v, %v", result, err)
	}
}

func TestActivationMissingAccountOrCatalogNeverSends(t *testing.T) {
	for _, mode := range []string{"disabled", "provider", "index", "catalog", "no-models", "key", "external-management"} {
		t.Run(mode, func(t *testing.T) {
			cfg, host, a, f := activationTestModel(t)
			switch mode {
			case "disabled":
				a.Disabled = true
			case "provider":
				a.Provider = "antigravity"
			case "index":
				a.AuthIndex = ""
			case "catalog":
				f.catalog = "invalid"
			case "no-models":
				f.registered = []string{"gpt-5-spark", "codex-auto-review", "gpt-image-2"}
			case "key":
				cfg.ManagementKeyFile = filepath.Join(t.TempDir(), "missing.key")
			case "external-management":
				cfg.ManagementURL = "https://example.invalid"
			}
			result, err := executeActivation(context.Background(), host, cfg, a, "")
			if err == nil || result.Ambiguous || result.Confirmed || f.posts != 0 {
				t.Fatalf("preflight failure sent request: %#v err=%v posts=%d", result, err, f.posts)
			}
		})
	}
}

func TestActivationTestEndpointNeverSendsToRealUpstream(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	cfg.TestProbeBase = "http://127.0.0.1:12345"
	_, err := executeActivation(context.Background(), host, cfg, a, "")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.request.URL != cfg.TestProbeBase+"/codex-activate/"+a.Name || f.request.Proxy != "direct" {
		t.Fatalf("test mode must override endpoint and proxy: %#v", f.request)
	}
}

func TestActivationFailuresAreUncertainSanitizedAndNeverRetried(t *testing.T) {
	for _, mode := range []string{"http", "management", "truncated", "missing-usage", "incomplete", "model-mismatch", "invalid-envelope"} {
		t.Run(mode, func(t *testing.T) {
			cfg, host, a, f := activationTestModel(t)
			switch mode {
			case "http":
				f.status, f.response = 429, `{"error":"Bearer fake-access-token; activation-management-secret"}`
			case "management":
				f.management = 502
			case "truncated":
				f.response = "data: {\"type\":\"response.created\"}\n\n"
			case "missing-usage":
				f.response = `{"status":"completed"}`
			case "incomplete":
				f.response = "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"
			case "model-mismatch":
				f.response = `{"status":"completed","model":"gpt-5-spark","usage":{"input_tokens":1,"output_tokens":1}}`
			case "invalid-envelope":
				f.response = "<html>fake-access-token activation-management-secret</html>"
			}
			result, err := executeActivation(context.Background(), host, cfg, a, "")
			if err == nil || !result.Ambiguous || result.Confirmed || f.posts != 1 {
				t.Fatalf("result=%#v error=%v posts=%d", result, err, f.posts)
			}
			for _, secret := range []string{"fake-access-token", "activation-management-secret", "<html>"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %s", secret)
				}
			}
		})
	}
}

func TestActivationCancellationBeforeSend(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := executeActivation(ctx, host, cfg, a, "")
	if !errors.Is(err, context.Canceled) || result.Ambiguous || f.posts != 0 {
		t.Fatalf("pre-send cancellation result=%#v err=%v posts=%d", result, err, f.posts)
	}
}

func TestActivationCancellationAfterSendIsUncertain(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	f.started, f.cancel = make(chan struct{}), true
	ctx, cancel := context.WithCancel(context.Background())
	type finished struct {
		result activationCallResult
		err    error
	}
	done := make(chan finished, 1)
	go func() {
		result, err := executeActivation(ctx, host, cfg, a, "")
		done <- finished{result, err}
	}()
	select {
	case <-f.started:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("activation did not start")
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || !r.result.Ambiguous || r.result.Confirmed {
			t.Fatalf("post-send cancellation result=%#v err=%v", r.result, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not interrupt the request")
	}
}

func TestActivationDeadlineAfterSendIsUncertain(t *testing.T) {
	cfg, host, a, f := activationTestModel(t)
	f.started, f.cancel = make(chan struct{}), true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := executeActivation(ctx, host, cfg, a, "")
	if !errors.Is(err, context.DeadlineExceeded) || !result.Ambiguous || result.Confirmed {
		t.Fatalf("post-send deadline result=%#v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatalf("deadline must not retry: %d requests", f.posts)
	}
}

func TestParseActivationTerminalAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"json", `{"status":"completed","usage":{"input_tokens":0,"output_tokens":1}}`, true},
		{"multiline-sse", "data: {\"type\":\"response.completed\",\n" + "data: \"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":0,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n", true},
		{"missing-input", `{"status":"completed","usage":{"output_tokens":1}}`, false},
		{"negative", `{"status":"completed","usage":{"input_tokens":-1,"output_tokens":1}}`, false},
		{"fraction", `{"status":"completed","usage":{"input_tokens":0.1,"output_tokens":1}}`, false},
		{"done-only", "data: [DONE]\n\n", false},
		{"only-started", "data: {\"type\":\"response.created\"}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := activationCallResult{Model: "gpt-6-luna"}
			err := parseActivationResponse([]byte(tc.raw), &result)
			if (err == nil) != tc.ok {
				t.Fatalf("parse result=%#v err=%v want success=%v", result, err, tc.ok)
			}
		})
	}
}
