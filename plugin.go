package main

import (
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed web/index.html web/style.css web/ui-model.js web/app.js
var webFiles embed.FS

var active struct {
	sync.RWMutex
	e *engine
}
var host hostCall

func main() {}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	var cfg runtimeConfig
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		return err
	}
	e, err := newEngine(cfg, host)
	if err != nil {
		return err
	}
	active.Lock()
	old := active.e
	active.e = e
	active.Unlock()
	if old != nil {
		close(old.stop)
		<-old.done
		// The retiring worker may have committed a transaction after newEngine
		// read the journal. Re-read only after it stops, before any new POST.
		journal, journalErr := loadResetJournal(e.cfg.StateDir)
		e.resetMu.Lock()
		e.resetJournal = journal
		e.resetJournalError = ""
		if journalErr != nil {
			e.resetJournalError = "重置卡执行记录读取失败；自动使用已暂停，额度保护继续运行"
		}
		e.resetMu.Unlock()
	}
	go e.run()
	return nil
}

func registration() any {
	return map[string]any{"schema_version": 6, "metadata": map[string]any{
		"Name": "账号额度保护", "Version": version, "Author": "Wuse777", "GitHubRepository": "https://github.com/Wuse777/cpa-quota-guard",
		"ConfigFields": []any{
			map[string]any{"Name": "state_dir", "Type": "string", "Description": "策略文件目录；CPA 服务用户必须有写入权限。默认 /var/lib/cliproxyapi/quota-guard"},
			map[string]any{"Name": "proxy_url", "Type": "string", "Description": "额度查询代理，例如 http://127.0.0.1:7890；空值直连，账号自带代理优先"},
			map[string]any{"Name": "management_url", "Type": "string", "Description": "CPA 本机管理地址，仅允许回环 HTTP；默认 http://127.0.0.1:8317"},
			map[string]any{"Name": "notification_webhook_file", "Type": "string", "Description": "可选通知Webhook URL私有文件，0600；为空禁用通知"},
			map[string]any{"Name": "management_key_file", "Type": "string", "Description": "CPA 管理密钥私有文件；安装器以 0600 权限配置，不在策略或状态中返回"},
		},
	}, "capabilities": map[string]bool{"scheduler": true, "scheduler_across_priorities": true, "management_api": true}}
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registration())
	case "management.register":
		return okEnvelope(map[string]any{
			"resources": []any{map[string]any{"Path": "/index", "Menu": "账号额度保护", "Description": "按账号与额度窗口设置保留比例、查看保护状态"}},
			"routes": []any{
				map[string]any{"Method": "GET", "Path": "/plugins/quota-guard/status"},
				map[string]any{"Method": "PUT", "Path": "/plugins/quota-guard/policy"},
				map[string]any{"Method": "POST", "Path": "/plugins/quota-guard/refresh"},
			},
		})
	}
	active.RLock()
	e := active.e
	active.RUnlock()
	if e == nil {
		return nil, errors.New("插件尚未初始化")
	}
	switch method {
	case "scheduler.pick":
		var req pickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(e.pick(req))
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(e.management(req))
	default:
		return nil, errors.New("不支持的插件方法")
	}
}

func (e *engine) management(req managementRequest) managementResponse {
	// Only static HTML is public. Account information and every mutation use management-authenticated routes.
	if req.Method == "GET" && strings.HasSuffix(req.Path, "/index") {
		body, _ := webFiles.ReadFile("web/index.html")
		css, _ := webFiles.ReadFile("web/style.css")
		js, _ := webFiles.ReadFile("web/app.js")
		uiModel, _ := webFiles.ReadFile("web/ui-model.js")
		js = append(append(uiModel, []byte("\n")...), js...)
		body = []byte(strings.ReplaceAll(strings.ReplaceAll(string(body), "/* QUOTA_STYLE */", string(css)), "/* QUOTA_SCRIPT */", string(js)))
		return managementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"}}, Body: body}
	}
	// The host authenticates these registered routes before dispatching to the plugin.
	base := "/v0/management/plugins/quota-guard/"
	if !strings.HasPrefix(req.Path, base) {
		return jsonResponse(404, map[string]string{"error": "接口不存在"})
	}
	switch req.Method + " " + strings.TrimPrefix(req.Path, base) {
	case "GET status":
		return jsonResponse(200, e.status())
	case "POST refresh":
		e.notifyRefresh()
		return jsonResponse(202, map[string]string{"status": "额度刷新已排队"})
	case "PUT policy":
		if len(req.Body) > 1024*1024 {
			return jsonResponse(413, map[string]string{"error": "策略过大"})
		}
		var p policy
		if err := decodeJSON(req.Body, &p); err != nil {
			return jsonResponse(400, map[string]string{"error": "策略 JSON 格式无效"})
		}
		if err := e.updatePolicy(p); err != nil {
			code := 400
			if errors.Is(err, errRevision) {
				code = 409
			}
			return jsonResponse(code, map[string]string{"error": err.Error()})
		}
		return jsonResponse(200, e.status())
	}
	return jsonResponse(404, map[string]string{"error": "接口不存在"})
}

func jsonResponse(code int, v any) managementResponse {
	raw, _ := json.Marshal(v)
	return managementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: raw}
}
