package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const activationResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

type activationModelError struct {
	message   string
	uncertain bool
	cause     error
}

func (e *activationModelError) Error() string             { return e.message }
func (e *activationModelError) ActivationUncertain() bool { return e.uncertain }
func (e *activationModelError) Unwrap() error             { return e.cause }

func activationFailure(message string, uncertain bool, cause error) error {
	return &activationModelError{message: message, uncertain: uncertain, cause: cause}
}

type activationModelDefinition struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	OutputModalities []string `json:"supportedOutputModalities"`
	Thinking         *struct {
		Levels      []string `json:"levels"`
		ZeroAllowed bool     `json:"zero_allowed"`
	} `json:"thinking"`
	Config *struct {
		OverrideHeader map[string]string `json:"override_header"`
	} `json:"config"`
}

type activationModelChoice struct {
	activationModel
	headers map[string]string
}

// Only canonical Codex text models can start the normal five-hour/week pool.
// A registered alias alone does not establish its upstream model or quota pool.
func activationNormalModel(id string) bool {
	name := strings.ToLower(id)
	if len(id) > 120 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\() \t\r\n") {
		return false
	}
	if !strings.HasPrefix(name, "gpt-") && name != "codex-mini-latest" {
		return false
	}
	for _, excluded := range []string{"spark", "review", "image", "audio", "embedding", "transcrib", "realtime", "tts", "search", "deep-research"} {
		if strings.Contains(name, excluded) {
			return false
		}
	}
	return true
}

func activationLowestEffort(d activationModelDefinition) (string, bool) {
	if d.Thinking == nil {
		return "", true
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		for _, supported := range d.Thinking.Levels {
			if supported == effort {
				return effort, true
			}
		}
	}
	if d.Thinking.ZeroAllowed {
		return "none", true
	}
	// Unknown reasoning capabilities must not silently select a server default.
	return "", false
}

func activationModelRank(id string) int {
	name := strings.ToLower(id)
	switch {
	case strings.Contains(name, "nano"):
		return 0
	case strings.Contains(name, "mini"):
		return 10
	case strings.Contains(name, "luna"):
		return 20
	case strings.Contains(name, "terra"):
		return 30
	case strings.Contains(name, "pro"), strings.Contains(name, "max"), strings.Contains(name, "astra"):
		return 70
	case strings.Contains(name, "sol"):
		return 60
	default:
		return 40
	}
}

func activationModels(host hostCall, cfg runtimeConfig, a account) ([]activationModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return loadActivationModels(ctx, cfg, a)
}

func loadActivationModels(ctx context.Context, cfg runtimeConfig, a account) ([]activationModel, error) {
	choices, err := loadActivationChoices(ctx, cfg, a)
	if err != nil {
		return nil, err
	}
	options := make([]activationModel, 0, len(choices))
	for _, choice := range choices {
		options = append(options, choice.activationModel)
	}
	return options, nil
}

func loadActivationChoices(ctx context.Context, cfg runtimeConfig, a account) ([]activationModelChoice, error) {
	if a.Provider != "codex" || a.Disabled || a.ID == "" || a.AuthIndex == "" {
		return nil, activationFailure("账号不可用于额度周期激活", false, nil)
	}
	var registered struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := activationManagementJSON(ctx, cfg, "/v8/management/credentials/models?name="+url.QueryEscape(a.ID), &registered); err != nil {
		return nil, err
	}
	var catalog struct {
		Models []activationModelDefinition `json:"models"`
	}
	if err := activationManagementJSON(ctx, cfg, "/v8/management/routing/model-definitions/codex", &catalog); err != nil {
		return nil, err
	}
	available := make(map[string]bool, len(registered.Models))
	for _, m := range registered.Models {
		available[m.ID] = true
	}
	seen := map[string]bool{}
	options := []activationModelChoice{}
	for _, d := range catalog.Models {
		if !available[d.ID] || seen[d.ID] || !activationNormalModel(d.ID) || (d.Type != "" && d.Type != "openai" && d.Type != "codex") {
			continue
		}
		if len(d.OutputModalities) > 0 {
			text := false
			for _, modality := range d.OutputModalities {
				text = text || modality == "text"
			}
			if !text {
				continue
			}
		}
		effort, ok := activationLowestEffort(d)
		if !ok {
			continue
		}
		seen[d.ID] = true
		choice := activationModelChoice{activationModel: activationModel{ID: d.ID, LowestEffort: effort}, headers: map[string]string{}}
		if d.Config != nil {
			for key, value := range d.Config.OverrideHeader {
				name := http.CanonicalHeaderKey(key)
				if name != "User-Agent" && name != "Originator" && name != "Openai-Beta" {
					continue
				}
				if len(value) <= 512 && !strings.ContainsAny(value, "\r\n\x00") && !strings.Contains(value, "$TOKEN$") {
					choice.headers[name] = value
				}
			}
		}
		options = append(options, choice)
	}
	sort.Slice(options, func(i, j int) bool {
		ri, rj := activationModelRank(options[i].ID), activationModelRank(options[j].ID)
		if ri != rj {
			return ri < rj
		}
		return options[i].ID < options[j].ID
	})
	if len(options) == 0 {
		return nil, activationFailure("账号没有已核验的正常额度模型或推理档位", false, nil)
	}
	return options, nil
}

// Catalog reads never contain credentials and always use the loopback address.
func activationManagementJSON(ctx context.Context, cfg runtimeConfig, path string, dest any) error {
	if err := validateRuntime(cfg); err != nil {
		return activationFailure("额度激活的管理接口配置无效", false, nil)
	}
	keyFile := cfg.ManagementKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(cfg.StateDir, "cpa-management.key")
	}
	rawKey, err := os.ReadFile(keyFile)
	if err != nil {
		return activationFailure("无法读取 CPA 管理密钥文件", false, nil)
	}
	key := strings.TrimSpace(string(rawKey))
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return activationFailure("CPA 管理密钥文件无效", false, nil)
	}
	base := cfg.ManagementURL
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return activationFailure("模型目录请求构建失败", false, nil)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return activationFailure("模型目录查询失败或已取消；尚未发送模型请求", false, ctx.Err())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || resp.StatusCode != http.StatusOK || json.Unmarshal(raw, dest) != nil {
		return activationFailure("模型目录响应无效；尚未发送模型请求", false, ctx.Err())
	}
	return nil
}

func executeActivation(ctx context.Context, host hostCall, cfg runtimeConfig, a account, preferred string) (activationCallResult, error) {
	var result activationCallResult
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return result, activationFailure("额度激活已取消；尚未发送模型请求", false, ctx.Err())
	}
	options, err := loadActivationChoices(ctx, cfg, a)
	if err != nil {
		return result, err
	}
	selected := options[0]
	if preferred != "" {
		found := false
		for _, option := range options {
			if option.ID == preferred {
				selected, found = option, true
				break
			}
		}
		if !found {
			return result, activationFailure("指定模型不可用于该账号的正常额度周期激活", false, nil)
		}
	}
	result.Model, result.Reasoning = selected.ID, selected.LowestEffort
	api, err := newResetAPI(host, cfg, a)
	if err != nil {
		return result, activationFailure("无法读取目标账号授权；尚未发送模型请求", false, nil)
	}
	defer api.client.CloseIdleConnections()
	api.base = activationResponsesURL
	if cfg.TestProbeBase != "" {
		api.base = cfg.TestProbeBase + "/codex-activate/" + url.PathEscape(a.Name)
	}
	api.headers["Accept"] = "text/event-stream"
	api.headers["Originator"] = "codex-tui"
	api.headers["X-Codex-Routing-Hint"] = "model=" + selected.ID
	for key, value := range selected.headers {
		api.headers[key] = value
	}
	if strings.Contains(api.headers["User-Agent"], "Mac OS") {
		sessionID, err := requestUUID()
		if err != nil {
			return result, activationFailure("无法创建激活请求会话；尚未发送模型请求", false, nil)
		}
		api.headers["Session_id"] = sessionID
	}
	// Codex subscription responses do not accept output-token hard limits.
	// A fresh, tiny request with no tools/history is the supported low-use path.
	payload := map[string]any{
		"model": selected.ID, "stream": true, "store": false,
		"instructions": "Reply OK only.",
		"input":        []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Hi"}}}},
	}
	if selected.LowestEffort != "" {
		payload["reasoning"] = map[string]string{"effort": selected.LowestEffort}
	}
	if ctx.Err() != nil {
		return result, activationFailure("额度激活已取消；尚未发送模型请求", false, ctx.Err())
	}
	// This forwarding route performs one POST for one auth_index, without the
	// executor's model/account retry loop. Its HTTP context cancels upstream I/O.
	raw, status, sent, err := activationModelPOST(ctx, api, payload)
	if err != nil {
		result.Ambiguous = sent
		if !sent {
			return result, err
		}
		return result, activationFailure("额度激活请求未确认；请查询额度后核实，不会自动重发", true, ctx.Err())
	}
	if status < 200 || status >= 300 {
		result.Ambiguous = true
		return result, activationFailure(fmt.Sprintf("额度激活接口 HTTP %d；不会自动重发", status), true, nil)
	}
	if err := parseActivationResponse(raw, &result); err != nil {
		result.Ambiguous = true
		return result, activationFailure("额度激活响应未确认完成和用量；不会自动重发", true, nil)
	}
	result.Confirmed = true
	return result, nil
}

func activationModelPOST(ctx context.Context, api *resetAPI, payload map[string]any) ([]byte, int, bool, error) {
	keyRaw, err := os.ReadFile(api.keyFile)
	if err != nil {
		return nil, 0, false, activationFailure("无法读取 CPA 管理密钥；尚未发送模型请求", false, nil)
	}
	key := strings.TrimSpace(string(keyRaw))
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return nil, 0, false, activationFailure("CPA 管理密钥无效；尚未发送模型请求", false, nil)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, false, activationFailure("额度激活请求编码失败；尚未发送模型请求", false, nil)
	}
	raw, err := json.Marshal(struct {
		AuthIndex string            `json:"authIndex"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Proxy     string            `json:"proxy_url,omitempty"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data"`
	}{api.authIndex, http.MethodPost, api.base, api.proxy, api.headers, string(data)})
	if err != nil {
		return nil, 0, false, activationFailure("额度激活转发编码失败；尚未发送模型请求", false, nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.managementURL+"/v8/management/requests/api-call", bytes.NewReader(raw))
	if err != nil {
		return nil, 0, false, activationFailure("额度激活转发请求无效；尚未发送模型请求", false, nil)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if ctx.Err() != nil {
		return nil, 0, false, activationFailure("额度激活已取消；尚未发送模型请求", false, ctx.Err())
	}
	resp, err := api.client.Do(req)
	if err != nil {
		return nil, 0, true, activationFailure("额度激活转发结果不确定", true, ctx.Err())
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || resp.StatusCode != http.StatusOK {
		return nil, 0, true, activationFailure("额度激活转发响应不确定", true, ctx.Err())
	}
	var envelope struct {
		Status int     `json:"status_code"`
		Body   *string `json:"body"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status < 100 || envelope.Status > 599 || envelope.Body == nil {
		return nil, 0, true, activationFailure("额度激活转发响应格式无效", true, nil)
	}
	return []byte(*envelope.Body), envelope.Status, true, nil
}

func parseActivationResponse(raw []byte, result *activationCallResult) error {
	type response struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Usage  *struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	accept := func(r response) error {
		if r.Status != "completed" || r.Usage == nil || r.Usage.InputTokens == nil || r.Usage.OutputTokens == nil || *r.Usage.InputTokens < 0 || *r.Usage.OutputTokens < 0 {
			return errors.New("missing completed usage")
		}
		if r.Model != "" && r.Model != result.Model {
			return errors.New("unexpected response model")
		}
		result.InputTokens, result.OutputTokens = *r.Usage.InputTokens, *r.Usage.OutputTokens
		return nil
	}
	if json.Valid(raw) {
		var r response
		if json.Unmarshal(raw, &r) == nil {
			return accept(r)
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	var data []string
	var completed *response
	process := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return nil
		}
		var event struct {
			Type     string   `json:"type"`
			Response response `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			return errors.New("invalid SSE event")
		}
		switch event.Type {
		case "response.completed":
			if completed != nil {
				return errors.New("duplicate terminal event")
			}
			copy := event.Response
			completed = &copy
		case "response.failed", "response.incomplete", "error":
			return errors.New("failed terminal event")
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := process(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return errors.New("SSE read failed")
	}
	if err := process(); err != nil {
		return err
	}
	if completed == nil {
		return errors.New("missing completed event")
	}
	return accept(*completed)
}
