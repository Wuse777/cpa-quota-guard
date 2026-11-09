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
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

type notificationStatus struct {
	Enabled    bool      `json:"enabled"`
	Health     string    `json:"health"`
	LastSentAt time.Time `json:"last_sent_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}
type notificationAttempt struct {
	Failures  int
	NextAt    time.Time
	UpdatedAt time.Time
}
type notificationEvent struct {
	Kind               string    `json:"kind"`
	AccountFingerprint string    `json:"account_fingerprint,omitempty"`
	OperationID        string    `json:"operation_id,omitempty"`
	Message            string    `json:"message"`
	Version            string    `json:"version"`
	At                 time.Time `json:"at"`
}

func accountFingerprint(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func (e *engine) notificationsView() notificationStatus {
	e.notificationMu.Lock()
	defer e.notificationMu.Unlock()
	s := e.notificationStatus
	s.Enabled = e.cfg.NotificationWebhookFile != ""
	if !s.Enabled {
		s.Health = "disabled"
	} else if s.Health == "" {
		s.Health = "ready"
	}
	return s
}

func readWebhook(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("无法读取通知地址私有文件")
	}
	// Windows does not expose POSIX mode bits; production Linux must enforce 0600.
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		return "", errors.New("通知地址私有文件权限必须为 0600")
	}
	if st.Size() > 8192 {
		return "", errors.New("通知地址私有文件过大")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("无法读取通知地址私有文件")
	}
	endpoint := strings.TrimSpace(string(raw))
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("通知地址格式无效")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return "", errors.New("通知地址须为 HTTPS（隔离测试可使用回环 HTTP）")
		}
	}
	return endpoint, nil
}

func sendNotification(ctx context.Context, path string, event notificationEvent) error {
	endpoint, err := readWebhook(path)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return errors.New("通知编码失败")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return errors.New("通知请求构建失败")
	}
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	// Never return net/http errors: they can contain the complete secret webhook URL.
	if err != nil {
		return errors.New("通知发送超时或连接失败")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("通知接收端 HTTP %d", response.StatusCode)
	}
	return nil
}

func (e *engine) notificationEvents(now time.Time) []notificationEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetMu.Lock()
	defer e.resetMu.Unlock()
	events := []notificationEvent{}
	add := func(kind, id, operation, message string) {
		events = append(events, notificationEvent{kind, accountFingerprint(id), operation, message, version, now})
	}
	for id, s := range e.snapshots {
		r := e.ruleFor(s.Account)
		if r != nil && r.Enabled && !s.Account.Disabled && (quotaHealth(s, e.p, now) == "stale" || (s.CheckedAt.IsZero() && e.probeRetry[id].Failures >= 2 && s.Error != "")) {
			add("quota_stale", id, "", "额度查询持续失败或数据已过期；请检查插件健康状态")
		}
	}
	for key, op := range e.resetJournal.Operations {
		if op.State == "pending" && op.Attempts >= 2 {
			add("redemption_pending", op.Identity, key, "重置卡兑换连续未确认，后台继续核实并复用原请求编号")
		}
		if op.State != "success" && op.State != "confirmed" && op.ExpiresAt.Sub(now) < time.Hour && op.ExpiresAt.After(now) && op.LastResult != nil && op.LastResult.Error != "" {
			add("redemption_expiring", op.Identity, key, "重置卡将在一小时内到期，最近兑换未确认成功")
		}
	}
	return events
}

func notificationKey(event notificationEvent) string {
	return event.Kind + "\x00" + event.AccountFingerprint + "\x00" + event.OperationID
}

func (e *engine) dispatchNotifications(ctx context.Context, now time.Time) {
	if e.cfg.NotificationWebhookFile == "" {
		return
	}
	events := e.notificationEvents(now)
	active := map[string]bool{}
	for _, event := range events {
		active[notificationKey(event)] = true
	}
	e.notificationMu.Lock()
	for key, attempt := range e.notificationAttempts {
		if !active[key] && now.Sub(attempt.UpdatedAt) > time.Hour {
			delete(e.notificationAttempts, key)
		}
	}
	e.notificationMu.Unlock()
	sent := 0
	for _, event := range events {
		if sent >= 8 {
			break
		}
		if ctx.Err() != nil {
			return
		}
		key := notificationKey(event)
		e.notificationMu.Lock()
		attempt := e.notificationAttempts[key]
		full := len(e.notificationAttempts) >= 4096 && attempt.UpdatedAt.IsZero()
		e.notificationMu.Unlock()
		if full {
			continue
		}
		if now.Before(attempt.NextAt) {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		sent++
		err := sendNotification(sendCtx, e.cfg.NotificationWebhookFile, event)
		cancel()
		e.notificationMu.Lock()
		attempt.UpdatedAt = now
		e.notificationStatus.Enabled = true
		if err == nil {
			attempt.Failures = 0
			attempt.NextAt = now.Add(30 * time.Minute)
			e.notificationStatus.Health = "healthy"
			e.notificationStatus.LastSentAt = now
			e.notificationStatus.LastError = ""
		} else {
			retry := nextRetry(retryState{Failures: attempt.Failures}, now)
			attempt.Failures = retry.Failures
			attempt.NextAt = retry.NextAt
			e.notificationStatus.Health = "error"
			e.notificationStatus.LastError = err.Error()
		}
		// Bounded even when administrators repeatedly replace account sets.
		if len(e.notificationAttempts) >= 4096 {
			for oldKey, old := range e.notificationAttempts {
				if oldKey != key && now.Sub(old.UpdatedAt) > time.Hour {
					delete(e.notificationAttempts, oldKey)
				}
			}
		}
		if len(e.notificationAttempts) < 4096 || e.notificationAttempts[key].UpdatedAt != (time.Time{}) {
			e.notificationAttempts[key] = attempt
		}
		e.notificationMu.Unlock()
	}
}

func (e *engine) runNotificationWorker(ctx context.Context) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			e.dispatchNotifications(ctx, time.Now())
			timer.Reset(15 * time.Second)
		}
	}
}
