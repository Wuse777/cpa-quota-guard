package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Scheduled activation uses a fixed Beijing civil-time contract. This keeps the
// meaning stable on hosts whose local timezone or tzdata differs.
var activationScheduleZone = time.FixedZone("Asia/Shanghai", 8*60*60)

const activationScheduleGrace = 2 * time.Minute

func validateActivationTimes(times []string) error {
	if len(times) > 24 {
		return errors.New("每日定时激活最多设置 24 个时间点")
	}
	seen := map[string]bool{}
	for _, value := range times {
		t, err := time.Parse("15:04", value)
		if err != nil || t.Format("15:04") != value {
			return errors.New("每日激活时间必须使用 HH:mm（00:00–23:59）")
		}
		if seen[value] {
			return fmt.Errorf("每日激活时间重复：%s", value)
		}
		seen[value] = true
	}
	return nil
}

func activationScheduleEnabled(p activationPolicy) bool {
	return p.Enabled && len(p.DailyTimes) > 0 && (p.FiveHour || p.Weekly)
}

func activationScheduleSlot(value string, date time.Time) (time.Time, bool) {
	t, err := time.Parse("15:04", value)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(date.Year(), date.Month(), date.Day(), t.Hour(), t.Minute(), 0, 0, activationScheduleZone), true
}

// Returns the currently due slot and the next future slot. A slot is eligible
// only during the short grace period; missed slots are deliberately not replayed.
func activationSchedule(p activationPolicy, now, claimed time.Time) (due, next time.Time) {
	if !activationScheduleEnabled(p) {
		return
	}
	local := now.In(activationScheduleZone)
	if claimed.After(local) { // A backward clock adjustment cannot reopen an old slot.
		local = claimed.In(activationScheduleZone)
	}
	for _, value := range p.DailyTimes {
		for day := -1; day <= 1; day++ {
			candidate, ok := activationScheduleSlot(value, local.AddDate(0, 0, day))
			if !ok || !candidate.After(claimed) {
				continue
			}
			if !candidate.After(now) && now.Before(candidate.Add(activationScheduleGrace)) {
				if due.IsZero() || candidate.Before(due) {
					due = candidate
				}
			} else if candidate.After(now) && (next.IsZero() || candidate.Before(next)) {
				next = candidate
			}
		}
	}
	return
}

func activationScheduleKey(slot time.Time) string {
	return slot.In(activationScheduleZone).Format("2006-01-02 15:04")
}

func activationScheduleOpen(p activationPolicy, slot, now time.Time) bool {
	if !activationScheduleEnabled(p) || now.Before(slot) || !now.Before(slot.Add(activationScheduleGrace)) {
		return false
	}
	for _, value := range p.DailyTimes {
		if value == slot.In(activationScheduleZone).Format("15:04") {
			return true
		}
	}
	return false
}

func scheduledActivationID(a account, identity string, slot time.Time) string {
	if identity == "" {
		identity = a.ID
	}
	return activationHash("daily\x00" + identity + "\x00" + activationScheduleKey(slot))
}

// Only consider windows with affirmative idle evidence. A stable future timer
// at 100% may be a reporting placeholder and is therefore not sufficient.
func (e *engine) scheduledActivationLocked(p activationPolicy, st *activationIdentity, s snapshot, identity string, slot, now time.Time) activationOperation {
	windows := []string{}
	cycles := map[string]string{}
	for _, b := range s.Buckets {
		k := activationKind(b)
		w, ok := st.Windows[k]
		// For an initial full window with a future timer, require the same
		// drift evidence as the refresh path (Boundary records the anchor).
		unverifiedTimer := strings.HasPrefix(w.Cycle, "initial:") && b.ResetAt.After(now) &&
			(w.ActiveUntil.IsZero() || w.Boundary != w.ActiveUntil.Unix())
		if _, duplicate := cycles[k]; duplicate || !ok || !activationSelected(p, k) || !w.CheckedAt.Equal(s.CheckedAt) || !activationIdle(b, now) ||
			w.Handled || w.AwaitingClock || w.ObservedUse || unverifiedTimer || e.activationCycleSentLocked(identity, k, w.Cycle) {
			continue
		}
		windows = append(windows, k)
		cycles[k] = w.Cycle
	}
	sort.Strings(windows)
	return activationOperation{
		activationEvent: activationEvent{ID: scheduledActivationID(s.Account, identity, slot), Account: s.Account.Name,
			At: now, Windows: windows, ScheduledAt: &slot, Reason: "每日定时启动尚未开始的额度窗口"},
		Identity: identity, Cycles: cycles,
	}
}

// Claim each identity before preflight. A restart can still process untouched
// identities within the grace period, but never replays a claimed one.
func (e *engine) claimScheduledIdentity(identity string, slot time.Time) bool {
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	if e.activationError != "" || !slot.After(e.activationJournal.completedSchedule()) {
		return false
	}
	st := e.activationJournal.Identities[identity]
	if st == nil {
		st = &activationIdentity{Windows: map[string]activationWindow{}}
		e.activationJournal.Identities[identity] = st
	}
	if st.LastScheduleAt != nil && !slot.After(*st.LastScheduleAt) {
		return false
	}
	st.LastScheduleAt = &slot
	return e.commitActivationLocked() == nil
}

func (e *engine) processScheduledActivations(ctx context.Context) {
	for ctx.Err() == nil {
		p, cfg, accounts := e.activationAccounts()
		if len(accounts) == 0 {
			return
		}
		e.activationMu.Lock()
		slot, _ := activationSchedule(p.AutoActivate, time.Now(), e.activationJournal.completedSchedule())
		journalOK := e.activationError == ""
		e.activationMu.Unlock()
		if !journalOK || slot.IsZero() {
			return
		}
		e.processScheduledSlot(ctx, p, cfg, accounts, slot)
		if ctx.Err() != nil {
			return
		}
		e.activationMu.Lock()
		e.activationJournal.CompletedScheduleAt = &slot
		err := e.commitActivationLocked()
		e.activationMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (e *engine) processScheduledSlot(ctx context.Context, p policy, cfg runtimeConfig, accounts []account, slot time.Time) {
	ctx, cancel := context.WithDeadline(ctx, slot.Add(activationScheduleGrace))
	defer cancel()
	seen := map[string]bool{}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		identity, err := activationAccountIdentity(e.host, a)
		if err != nil {
			e.skipScheduledActivation(a, "", slot, err.Error())
			continue
		}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		lock := e.resetIdentityLock(identity)
		lock.Lock()
		if ctx.Err() == nil && e.claimScheduledIdentity(identity, slot) {
			e.processActivation(ctx, p, cfg, a, identity, slot)
		}
		lock.Unlock()
	}
}

func (e *engine) skipScheduledActivation(a account, identity string, slot time.Time, reason string) {
	e.activationMu.Lock()
	defer e.activationMu.Unlock()
	if e.activationError != "" {
		return
	}
	e.activationJournal.Events = append(e.activationJournal.Events, activationEvent{
		ID: scheduledActivationID(a, identity, slot), Account: a.Name, At: time.Now(), State: "skipped",
		Reason: reason, Windows: []string{}, ScheduledAt: &slot,
	})
	_ = e.commitActivationLocked()
}

func (j activationJournal) completedSchedule() time.Time {
	if j.CompletedScheduleAt != nil {
		return *j.CompletedScheduleAt
	}
	return time.Time{}
}
