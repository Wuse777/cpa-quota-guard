package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

func defaultPolicy() policy {
	return policy{Revision: 1, Mode: "observe", PollSeconds: 60, MaxAgeSeconds: 180,
		TimeoutSeconds: 15, SafetyPercent: 3, ResumeGapPercent: 5, UnknownAction: "block",
		Strategy: "round-robin", Rules: []rule{}, AutoReset: resetPolicy{Enabled: false, IncludeNewAccounts: false, BeforeHours: 6, Accounts: map[string]bool{}}}
}

func validatePolicy(p policy) error {
	if math.IsNaN(p.AutoReset.BeforeHours) || math.IsInf(p.AutoReset.BeforeHours, 0) || p.AutoReset.BeforeHours < 0.1 || p.AutoReset.BeforeHours > 168 {
		return errors.New("重置卡提前时间须为 0.1–168 小时")
	}
	if len(p.AutoReset.Accounts) > 10000 {
		return errors.New("重置卡账号设置过多")
	}
	for id := range p.AutoReset.Accounts {
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || len(id) > 512 {
			return errors.New("重置卡账号标识无效")
		}
	}
	if p.Mode != "observe" && p.Mode != "enforce" {
		return errors.New("mode 必须为 observe 或 enforce")
	}
	if p.PollSeconds < 10 || p.PollSeconds > 3600 {
		return errors.New("检查间隔须为 10–3600 秒")
	}
	if p.MaxAgeSeconds < p.PollSeconds || p.MaxAgeSeconds > 86400 {
		return errors.New("数据有效期须不小于检查间隔，且不超过 86400 秒")
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 60 {
		return errors.New("查询超时须为 1–60 秒")
	}
	if !validPercent(p.SafetyPercent) || !validPercent(p.ResumeGapPercent) {
		return errors.New("安全余量及恢复间隔须为 0–100")
	}
	if p.UnknownAction != "block" && p.UnknownAction != "allow" {
		return errors.New("未知额度策略须为 block 或 allow")
	}
	if p.Strategy != "round-robin" && p.Strategy != "fill-first" {
		return errors.New("调度方式须为 round-robin 或 fill-first")
	}
	if len(p.Rules) > 10000 {
		return errors.New("账号规则过多")
	}
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if strings.TrimSpace(r.Account) == "" || r.Account != strings.TrimSpace(r.Account) || len(r.Account) > 512 {
			return errors.New("账号标识无效")
		}
		if seen[r.Account] {
			return fmt.Errorf("账号规则重复: %s", r.Account)
		}
		seen[r.Account] = true
		if !validPercent(r.MinRemainingPercent) {
			return errors.New("最低剩余额度须为 0–100")
		}
		if len(r.Windows) > 128 {
			return errors.New("单账号额度窗口过多")
		}
		for key, value := range r.Windows {
			if key == "" || len(key) > 512 || !validPercent(value) {
				return errors.New("额度窗口或阈值无效")
			}
		}
	}
	return nil
}

func validPercent(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100 }

func decodeJSON(raw []byte, dest any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("JSON 必须只包含一个对象")
	}
	return nil
}

func loadPolicy(dir string) (policy, error) {
	p := defaultPolicy()
	raw, err := os.ReadFile(filepath.Join(dir, "policy.json"))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	// Existing files predate safe onboarding: omitted fields retain their legacy behavior.
	p.AutoReset.Enabled = true
	p.AutoReset.IncludeNewAccounts = true
	if err = decodeJSON(raw, &p); err != nil {
		return p, fmt.Errorf("policy.json 无效: %w", err)
	}
	return p, validatePolicy(p)
}

// Commit on disk before publishing the new policy to request goroutines.
func savePolicy(dir string, p policy) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".policy-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "policy.json")); err != nil {
		return err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
