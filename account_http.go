package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// Account credentials are read afresh from CPA, never persisted by this plugin.
func accountHTTP(host hostCall, cfg runtimeConfig, a account) (*http.Client, http.Header, map[string]any, error) {
	var auth struct {
		JSON json.RawMessage `json:"json"`
	}
	if host == nil || host("host.auth.get", map[string]string{"auth_index": a.AuthIndex}, &auth) != nil {
		return nil, nil, nil, errors.New("无法读取账号授权；请检查 CPA 授权状态")
	}
	var credentials map[string]any
	if json.Unmarshal(auth.JSON, &credentials) != nil {
		return nil, nil, nil, errors.New("账号授权格式无效")
	}
	token := stringAt(credentials, "access_token")
	if token == "" {
		token = stringAt(objectAt(credentials, "token"), "access_token")
	}
	if token == "" {
		return nil, nil, nil, errors.New("账号没有可用的 access_token")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy := cfg.ProxyURL
	if custom := stringAt(credentials, "proxy_url", "proxy-url"); custom != "" {
		proxy = custom
	}
	if cfg.TestProbeBase != "" {
		proxy = "direct"
	}
	if proxy != "" && proxy != "direct" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, nil, nil, errors.New("账号代理地址无效")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	headers := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}, "Content-Type": {"application/json"}}
	if a.Provider == "codex" {
		headers.Set("User-Agent", "codex-tui/0.149.1")
		if id := stringAt(credentials, "account_id"); id != "" {
			headers.Set("Chatgpt-Account-Id", id)
		}
	}
	return client, headers, credentials, nil
}
