package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func validateRuntime(c runtimeConfig) error {
	if c.ManagementURL != "" {
		u, err := url.Parse(c.ManagementURL)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			return errors.New("management_url 仅允许无路径的回环 HTTP 地址")
		}
	}
	if c.ProxyURL != "" && c.ProxyURL != "direct" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return errors.New("proxy_url 无效")
		}
	}
	if c.TestProbeBase != "" {
		u, err := url.Parse(c.TestProbeBase)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			return errors.New("test_probe_base 仅允许无路径的回环 HTTP 地址")
		}
	}
	return nil
}

func probeAccount(ctx context.Context, host hostCall, cfg runtimeConfig, a account) ([]bucket, error) {
	client, headers, credentials, err := accountHTTP(host, cfg, a)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	var urls []string
	method := "GET"
	var body []byte
	switch a.Provider {
	case "codex":
		urls = []string{"https://chatgpt.com/backend-api/wham/usage"}
		headers.Set("User-Agent", "codex-tui/0.149.1")
		if id := stringAt(credentials, "account_id"); id != "" {
			headers.Set("Chatgpt-Account-Id", id)
		}
	case "antigravity":
		method = "POST"
		project := stringAt(credentials, "project_id", "projectId")
		if project == "" {
			return nil, errors.New("Antigravity 账号缺少 project_id")
		}
		body, _ = json.Marshal(map[string]string{"project": project})
		headers.Set("User-Agent", "antigravity/2.23.0 darwin/arm64")
		urls = []string{"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"}
	default:
		return nil, errors.New("服务商尚不支持")
	}
	if cfg.TestProbeBase != "" {
		urls = []string{cfg.TestProbeBase + "/" + a.Provider + "/" + url.PathEscape(a.Name)}
	}
	last := errors.New("额度查询失败")
	for _, endpoint := range urls {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("额度请求构建失败")
		}
		req.Header = headers
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errors.New("额度查询超时或已取消")
			}
			last = errors.New("额度查询网络失败，请检查代理与网络")
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			last = fmt.Errorf("额度接口 HTTP %d；请检查授权或稍后重试", resp.StatusCode)
			if resp.StatusCode == 401 {
				return nil, last
			}
			continue
		}
		if readErr != nil || len(raw) > 2*1024*1024 {
			last = errors.New("额度响应读取失败或过大")
			continue
		}
		buckets, parseErr := parseQuota(a.Provider, raw, time.Now())
		if parseErr == nil {
			return buckets, nil
		}
		last = parseErr
	}
	return nil, last
}

func parseQuota(provider string, raw []byte, now time.Time) ([]bucket, error) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("额度接口返回了无效 JSON")
	}
	var buckets []bucket
	if provider == "codex" {
		buckets = appendCodexWindows(buckets, "", objectAt(payload, "rate_limit", "rateLimit"), false, now)
		buckets = appendCodexWindows(buckets, "code-review/", objectAt(payload, "code_review_rate_limit", "codeReviewRateLimit"), true, now)
		additional := valueAt(payload, "additional_rate_limits", "additionalRateLimits")
		appendAdditional := func(name string, v map[string]any) {
			if name == "" {
				name = stringAt(v, "limit_name", "limitName", "name")
			}
			if name == "" {
				return
			}
			limits := objectAt(v, "rate_limit", "rateLimit")
			if limits == nil {
				limits = v
			}
			buckets = appendCodexWindows(buckets, "additional/"+name+"/", limits, true, now)
		}
		switch values := additional.(type) {
		case []any:
			for _, v := range values {
				if m, ok := v.(map[string]any); ok {
					appendAdditional("", m)
				}
			}
		case map[string]any:
			for k, v := range values {
				if m, ok := v.(map[string]any); ok {
					appendAdditional(k, m)
				}
			}
		}
	} else if provider == "antigravity" {
		if groups, ok := payload["groups"].([]any); ok {
			for _, g := range groups {
				group, ok := g.(map[string]any)
				if !ok {
					continue
				}
				name := stringAt(group, "groupId", "group_id", "displayName", "display_name")
				if name == "" {
					continue
				}
				values, _ := group["buckets"].([]any)
				for _, v := range values {
					b, ok := v.(map[string]any)
					if !ok {
						return nil, errors.New("额度窗口格式无效")
					}
					fraction, ok := numberAt(b, "remainingFraction", "remaining_fraction")
					if !ok || !validPercent(fraction*100) {
						return nil, errors.New("额度窗口缺少有效剩余比例")
					}
					id := stringAt(b, "bucketId", "bucket_id", "window")
					if id == "" {
						continue
					}
					label := stringAt(b, "displayName", "display_name", "description", "window")
					if label == "" {
						label = id
					}
					buckets = append(buckets, bucket{ID: name + "/" + id, Label: name + " · " + label, RemainingPercent: fraction * 100, ResetAt: parseReset(valueAt(b, "resetTime", "reset_time"), now)})
				}
			}
		}
		// Older fetchAvailableModels responses remain readable for compatible gateways.
		if models, ok := payload["models"].(map[string]any); ok && len(buckets) == 0 {
			for id, value := range models {
				m, ok := value.(map[string]any)
				if !ok {
					continue
				}
				q := objectAt(m, "quotaInfo", "quota_info")
				if q == nil {
					continue
				}
				fraction, ok := numberAt(q, "remainingFraction", "remaining_fraction")
				if !ok || !validPercent(fraction*100) {
					return nil, errors.New("额度窗口缺少有效剩余比例")
				}
				buckets = append(buckets, bucket{ID: id, Label: id, RemainingPercent: fraction * 100, ResetAt: parseReset(valueAt(q, "resetTime", "reset_time"), now)})
			}
		}
	}
	if len(buckets) == 0 {
		return nil, errors.New("没有可解析的额度窗口；不会假设额度已恢复")
	}
	seen := map[string]bool{}
	for _, b := range buckets {
		if !validPercent(b.RemainingPercent) {
			return nil, errors.New("额度窗口缺少有效剩余比例")
		}
		if seen[b.ID] {
			return nil, errors.New("额度窗口标识重复")
		}
		seen[b.ID] = true
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].ID < buckets[j].ID })
	return buckets, nil
}

func appendCodexWindows(out []bucket, prefix string, m map[string]any, aux bool, now time.Time) []bucket {
	for _, key := range []string{"primary", "secondary"} {
		w := objectAt(m, key+"_window", key+"Window")
		if w == nil {
			continue
		}
		used, ok := numberAt(w, "used_percent", "usedPercent")
		if !ok || !validPercent(used) {
			out = append(out, bucket{ID: prefix + key, RemainingPercent: -1})
			continue
		}
		remaining := 100 - used
		if allowed, ok := m["allowed"].(bool); ok && !allowed {
			remaining = 0
		}
		if reached, ok := valueAt(m, "limit_reached", "limitReached").(bool); ok && reached {
			remaining = 0
		}
		reset := parseReset(valueAt(w, "reset_at", "resetAt"), now)
		if reset.IsZero() {
			if seconds, ok := numberAt(w, "reset_after_seconds", "resetAfterSeconds"); ok && seconds >= 0 {
				reset = now.Add(time.Duration(seconds * float64(time.Second)))
			}
		}
		label := key
		if seconds, ok := numberAt(w, "limit_window_seconds", "limitWindowSeconds"); ok {
			label = fmt.Sprintf("%s · %.0f 小时", key, seconds/3600)
		}
		out = append(out, bucket{ID: prefix + key, Label: prefix + label, RemainingPercent: remaining, ResetAt: reset, Auxiliary: aux})
	}
	return out
}

func valueAt(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}
func objectAt(m map[string]any, keys ...string) map[string]any {
	v, _ := valueAt(m, keys...).(map[string]any)
	return v
}
func stringAt(m map[string]any, keys ...string) string {
	v, _ := valueAt(m, keys...).(string)
	return strings.TrimSpace(v)
}
func numberAt(m map[string]any, keys ...string) (float64, bool) {
	switch v := valueAt(m, keys...).(type) {
	case float64:
		return v, true
	case string:
		f, e := strconv.ParseFloat(v, 64)
		return f, e == nil
	}
	return 0, false
}
func parseReset(v any, now time.Time) time.Time {
	switch t := v.(type) {
	case float64:
		if t > 1e12 {
			return time.UnixMilli(int64(t))
		}
		if t > 0 {
			return time.Unix(int64(t), 0)
		}
	case string:
		if parsed, e := time.Parse(time.RFC3339Nano, t); e == nil {
			return parsed
		}
		if n, e := strconv.ParseFloat(t, 64); e == nil {
			return parseReset(n, now)
		}
	}
	return time.Time{}
}
