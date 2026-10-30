package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const resetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// Match CPAMC's manual reset: CPA resolves $TOKEN$ and owns the upstream transport.
type resetAPI struct {
	client                                                   *http.Client
	headers                                                  map[string]string
	base, managementURL, keyFile, identity, authIndex, proxy string
	secrets                                                  []string
	managementStatus                                         int
}

func newResetAPI(host hostCall, cfg runtimeConfig, a account) (*resetAPI, error) {
	credentials, err := accountCredentials(host, a)
	if err != nil {
		return nil, err
	}
	if a.AuthIndex == "" {
		return nil, errors.New("账号缺少 CPA auth_index，无法通过宿主兑换")
	}
	identity := stringAt(credentials, "account_id")
	if identity == "" {
		identity = a.ID
	}
	if identity == "" {
		identity = a.Name
	}
	sum := sha256.Sum256([]byte("codex\x00" + identity))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Never send the management key through a proxy.
	api := &resetAPI{
		client:  &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		headers: map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"},
		base:    resetCreditsURL, managementURL: cfg.ManagementURL, keyFile: cfg.ManagementKeyFile,
		identity: hex.EncodeToString(sum[:]), authIndex: a.AuthIndex, proxy: cfg.ProxyURL,
	}
	if api.managementURL == "" {
		api.managementURL = "http://127.0.0.1:8317"
	}
	if api.keyFile == "" {
		api.keyFile = filepath.Join(cfg.StateDir, "cpa-management.key")
	}
	if id := stringAt(credentials, "account_id"); id != "" {
		api.headers["Chatgpt-Account-Id"] = id
	}
	if custom := stringAt(credentials, "proxy_url", "proxy-url"); custom != "" {
		api.proxy = custom
	}
	if cfg.TestProbeBase != "" {
		api.base = cfg.TestProbeBase + "/codex-reset/" + url.PathEscape(a.Name)
		api.proxy = "direct"
	}
	collectResetSecrets(credentials, &api.secrets)
	return api, nil
}

func (api *resetAPI) call(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	api.managementStatus = 0
	keyRaw, err := os.ReadFile(api.keyFile)
	if err != nil {
		return nil, 0, errors.New("无法读取 CPA 管理密钥文件；请使用新版安装器配置后台兑换")
	}
	key := strings.TrimSpace(string(keyRaw))
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return nil, 0, errors.New("CPA 管理密钥文件无效")
	}
	api.secrets = append(api.secrets, key)
	headers := make(map[string]string, len(api.headers)+3)
	for k, v := range api.headers {
		headers[k] = v
	}
	if method == http.MethodGet {
		headers["Accept"], headers["OpenAI-Beta"], headers["Originator"] = "application/json", "codex-1", "Codex Desktop"
	}
	data := ""
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, errors.New("重置卡请求编码失败")
		}
		data = string(raw)
	}
	payload, _ := json.Marshal(struct {
		AuthIndex string            `json:"authIndex"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Proxy     string            `json:"proxy_url,omitempty"`
		Header    map[string]string `json:"header"`
		Data      string            `json:"data,omitempty"`
	}{api.authIndex, method, api.base + path, api.proxy, headers, data})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.managementURL+"/v8/management/requests/api-call", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, errors.New("CPA 重置卡请求构建失败")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	response, err := api.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("CPA 重置卡转发超时或连接失败；兑换结果未确认")
	}
	defer response.Body.Close()
	api.managementStatus = response.StatusCode
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return nil, 0, errors.New("CPA 重置卡响应读取失败")
	}
	if response.StatusCode != http.StatusOK {
		summary := api.errorSummary(raw)
		return nil, 0, fmt.Errorf("CPA 转发接口 HTTP %d%s", response.StatusCode, summary)
	}
	var envelope struct {
		Status int     `json:"status_code"`
		Body   *string `json:"body"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status < 100 || envelope.Status > 599 || envelope.Body == nil {
		return nil, 0, errors.New("CPA 转发响应格式无效，无法确认上游结果")
	}
	return []byte(*envelope.Body), envelope.Status, nil
}

func (api *resetAPI) list(ctx context.Context) ([]resetCredit, *int, error) {
	raw, status, err := api.call(ctx, http.MethodGet, "", nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("重置卡查询 HTTP %d%s", status, api.errorSummary(raw))
	}
	return parseResetCredits(raw)
}

type resetResult struct {
	At               time.Time `json:"at"`
	ManagementStatus int       `json:"management_status,omitempty"`
	HTTPStatus       int       `json:"http_status,omitempty"`
	Code             string    `json:"code,omitempty"`
	Error            string    `json:"error,omitempty"`
}

var resetCodePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,80}$`)
var resetBearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"'<>]+`)
var resetTokenPattern = regexp.MustCompile(`\b(?:sk-[a-zA-Z0-9_-]+|eyJ[a-zA-Z0-9_.-]{16,})`)

func collectResetSecrets(value map[string]any, secrets *[]string) {
	for k, v := range value {
		key := strings.ToLower(k)
		if nested, ok := v.(map[string]any); ok {
			collectResetSecrets(nested, secrets)
		}
		if text, ok := v.(string); ok && text != "" && (strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "cookie") || strings.Contains(key, "api_key") || key == "authorization") {
			*secrets = append(*secrets, text)
		}
	}
}

func (api *resetAPI) safeSummary(text string) string {
	for _, secret := range api.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = resetBearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = resetTokenPattern.ReplaceAllString(text, "[REDACTED]")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > 512 {
		runes = append(runes[:512], '…')
	}
	return string(runes)
}

// Store only bounded, sanitized structured errors, never raw bodies or headers.
func (api *resetAPI) errorSummary(raw []byte) string {
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	message := payload.Message
	var text string
	if json.Unmarshal(payload.Error, &text) == nil && text != "" {
		message = text
	} else {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload.Error, &nested) == nil && nested.Message != "" {
			message = nested.Message
		}
	}
	if message = api.safeSummary(message); message != "" {
		return "；" + message
	}
	return ""
}

func (api *resetAPI) result(raw []byte, status int, callErr error) resetResult {
	r := resetResult{At: time.Now(), ManagementStatus: api.managementStatus, HTTPStatus: status}
	if callErr != nil {
		r.Error = api.safeSummary(callErr.Error())
		return r
	}
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		r.Error = fmt.Sprintf("兑换接口 HTTP %d；响应不是有效 JSON 对象，结果未确认", status)
		return r
	}
	if resetCodePattern.MatchString(payload.Code) {
		r.Code = api.safeSummary(payload.Code)
	}
	if status >= 200 && status < 300 && (payload.Code == "reset" || payload.Code == "already_redeemed") {
		return r
	}
	code := r.Code
	if code == "" {
		var nested struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &nested) == nil && resetCodePattern.MatchString(nested.Error.Code) {
			r.Code = api.safeSummary(nested.Error.Code)
		}
		code = "未返回有效的顶层 code"
	}
	r.Error = fmt.Sprintf("兑换接口 HTTP %d；%s%s；未确认重置", status, code, api.errorSummary(raw))
	return r
}
