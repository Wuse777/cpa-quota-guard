package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDailyActivationScheduleValidation(t *testing.T) {
	for _, times := range [][]string{nil, {}, {"19:00", "00:00", "23:59"}} {
		if err := validateActivationTimes(times); err != nil {
			t.Fatalf("%v: %v", times, err)
		}
	}
	for _, times := range [][]string{{"9:00"}, {"12:00 "}, {"24:00"}, {"16:60"}, {"16:00", "16:00"}, make([]string, 25)} {
		if err := validateActivationTimes(times); err == nil {
			t.Fatalf("accepted invalid schedule: %q", times)
		}
	}
}

func TestDailyActivationSlotsInBeijingTime(t *testing.T) {
	p := activationPolicy{Enabled: true, FiveHour: true, DailyTimes: []string{"16:00", "00:00", "23:59"}}
	midnight := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	beforeMidnight := midnight.Add(-time.Minute)
	afternoon := midnight.Add(16 * time.Hour)
	for _, tt := range []struct {
		name                      string
		now, completed, due, next time.Time
	}{
		{"before first slot", beforeMidnight.Add(-time.Second), time.Time{}, time.Time{}, beforeMidnight},
		{"oldest open slot first across midnight", midnight, time.Time{}, beforeMidnight, afternoon},
		{"once previous slot is done", midnight, beforeMidnight, midnight, afternoon},
		{"short wake delay allowed", midnight.Add(119 * time.Second), beforeMidnight, midnight, afternoon},
		{"no replay after grace period", midnight.Add(120 * time.Second), time.Time{}, time.Time{}, afternoon},
		{"clock rollback cannot replay", midnight.Add(-3 * time.Hour), midnight, time.Time{}, afternoon},
		{"later slot can run same day", afternoon, midnight, afternoon, midnight.Add(23*time.Hour + 59*time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			due, next := activationSchedule(p, tt.now, tt.completed)
			if !due.Equal(tt.due) || !next.Equal(tt.next) {
				t.Fatalf("due=%s next=%s, want due=%s next=%s", due, next, tt.due, tt.next)
			}
		})
	}
	if key := activationScheduleKey(midnight); key != "2026-11-20 00:00" {
		t.Fatalf("wrong Beijing date: %s", key)
	}
	if activationScheduleOpen(p, midnight, midnight.Add(-time.Second)) || activationScheduleOpen(p, midnight, midnight.Add(2*time.Minute)) {
		t.Fatal("slot opened outside grace period")
	}
	p.DailyTimes = []string{"16:00"}
	if activationScheduleOpen(p, midnight, midnight) || !activationScheduleOpen(p, afternoon, afternoon) {
		t.Fatal("in-flight slot did not respect the edited schedule")
	}
	p.Enabled = false
	if due, next := activationSchedule(p, afternoon, time.Time{}); !due.IsZero() || !next.IsZero() {
		t.Fatal("disabled policy must not schedule")
	}
}

func TestDailyActivationWakeDelay(t *testing.T) {
	e, p, a, _, now := activationCoreFixture(t)
	p.Natural = false
	p.DailyTimes = []string{"09:00", "19:00"}
	e.p.AutoActivate = p
	e.snapshots[a.ID] = snapshot{Account: a}
	if got := e.activationDelay(now.Add(-3 * time.Second)); got != 3*time.Second {
		t.Fatalf("next slot must shorten wake delay, got %s", got)
	}
	if got := e.activationDelay(now); got != 0 {
		t.Fatalf("due slot must wake immediately, got %s", got)
	}
	e.activationJournal.CompletedScheduleAt = &now
	if got := e.activationDelay(now); got != time.Minute {
		t.Fatalf("completed slot must not busy-loop, got %s", got)
	}
}

func TestDailyActivationRequiresEvidenceOfIdleWindow(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.Natural, p.Reset = false, false
	p.DailyTimes = []string{"09:00", "16:00"}
	five := activationWindowTest(18000, 100, now.Add(5*time.Hour))
	week := activationWindowTest(604800, 70, now.Add(48*time.Hour))
	week.ID = "secondary"
	pick := func(at time.Time, b ...bucket) activationOperation {
		s := snapshot{Account: a, CheckedAt: at, Buckets: b}
		e.observeActivationLocked(id, s, p, at)
		return e.scheduledActivationLocked(p, e.activationJournal.Identities[id], s, id, now, at)
	}
	if op := pick(now, five, week); len(op.Windows) != 0 {
		t.Fatalf("initial future timer and used weekly window are not idle evidence: %+v", op)
	}
	five.ResetAt = five.ResetAt.Add(7 * time.Second)
	week.RemainingPercent = 100 // No reset-time change: business still owns this window.
	op := pick(now.Add(7*time.Second), five, week)
	if !reflect.DeepEqual(op.Windows, []string{"five_hour"}) || op.ScheduledAt == nil || !op.ScheduledAt.Equal(now) {
		t.Fatalf("only the moving full window should be eligible: %+v", op)
	}
	st := e.activationJournal.Identities[id]
	if st.Pending != "" {
		t.Fatal("schedule-only observation unexpectedly enqueued a reset or natural activation")
	}
	op.State = "uncertain"
	e.activationJournal.Operations[op.ID] = op
	settleActivationWindows(st, op)
	five.ResetAt = five.ResetAt.Add(9 * time.Second)
	if got := pick(now.Add(16*time.Second), five, week); len(got.Windows) != 0 {
		t.Fatalf("uncertain call cannot reopen its cycle or activate the other full window again: %+v", got)
	}
}

func TestDailyActivationCoalescesWithRefreshTriggers(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.DailyTimes = []string{"09:00"}
	five := activationWindowTest(18000, 10, now)
	week := activationWindowTest(604800, 30, now.Add(8*time.Hour))
	week.ID = "secondary"
	activationObserveTest(e, p, a, id, now.Add(-time.Second), five, week)
	five.RemainingPercent, week.RemainingPercent = 100, 100
	five.ResetAt, week.ResetAt = now.Add(5*time.Hour), now.Add(7*24*time.Hour)
	at := now.Add(time.Second)
	s := snapshot{Account: a, CheckedAt: at, Buckets: []bucket{five, week}}
	e.observeActivationLocked(id, s, p, at)
	pending := activationPendingTest(t, e, id)
	st := e.activationJournal.Identities[id]
	op := e.scheduledActivationLocked(p, st, s, id, now, at)
	if !reflect.DeepEqual(op.Windows, []string{"five_hour", "weekly"}) || !reflect.DeepEqual(op.Cycles, pending.Cycles) {
		t.Fatalf("one scheduled call should cover both refresh windows: scheduled=%+v pending=%+v", op, pending)
	}
	op.State = "succeeded"
	e.activationJournal.Operations[op.ID] = op
	settleActivationWindows(st, op)
	e.queueActivationLocked(id, a, p, at)
	if st.Pending != "" || e.activationJournal.Operations[pending.ID].State != "skipped" {
		t.Fatal("refresh task overlapped by the scheduled call must not be dispatched")
	}
}

func TestDailyActivationRecoversInFlightJournal(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.DailyTimes = []string{"09:00"}
	b := activationWindowTest(18000, 100, time.Time{})
	s := snapshot{Account: a, CheckedAt: now, Buckets: []bucket{b}}
	e.observeActivationLocked(id, s, p, now)
	st := e.activationJournal.Identities[id]
	st.LastScheduleAt = &now
	previous := now.Add(-24 * time.Hour)
	e.activationJournal.CompletedScheduleAt = &previous
	op := e.scheduledActivationLocked(p, st, s, id, now, now)
	op.State = "sending"
	e.activationJournal.Operations[op.ID] = op
	// Simulate the persisted preflight state with a process exit during sending.
	raw, err := json.Marshal(e.activationJournal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "activation-journal.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	e.initActivation()
	if e.activationError != "" {
		t.Fatal(e.activationError)
	}
	st = e.activationJournal.Identities[id]
	if recovered := e.activationJournal.Operations[op.ID]; recovered.State != "uncertain" || !st.Windows["five_hour"].Handled {
		t.Fatalf("in-flight slot did not recover conservatively: %+v", recovered)
	}
	if !e.activationJournal.completedSchedule().Equal(previous) || e.claimScheduledIdentity(id, now) {
		t.Fatal("claimed account must not be replayed even when the slot as a whole is incomplete")
	}
	if len(e.activationJournal.Events) != 1 || e.activationJournal.Events[0].ScheduledAt == nil || !e.activationJournal.Events[0].ScheduledAt.Equal(now) {
		t.Fatal("recovery must retain the original scheduled time in the timeline")
	}
}

func TestDailyActivationDispatchWithMockAccounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production activation journal requires Linux directory fsync")
	}
	for _, tt := range []struct {
		name       string
		five, week int
		probeFail  bool
		ambiguous  bool
		wantState  string
		wantPosts  int
	}{
		{"send once and resume from persisted identity claim", 0, 0, false, false, "succeeded", 1},
		{"keep uncertain outcome without replay", 0, 0, false, true, "uncertain", 1},
		{"skip starts with active use", 30, 30, false, false, "skipped", 0},
		{"skip when the other window is depleted", 0, 100, false, false, "skipped", 0},
		{"skip when quota is unknown", 0, 0, true, false, "skipped", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, fakeHost, a, f := activationTestModel(t)
			if tt.ambiguous {
				f.response = "event: response.created\ndata: {}\n\n"
			}
			alias := a
			alias.ID, alias.Name = "same-identity", "alias.json"
			host := func(method string, payload, dest any) error {
				if method == "host.auth.list" {
					raw, _ := json.Marshal(map[string]any{"files": []account{a, alias}})
					return json.Unmarshal(raw, dest)
				}
				return fakeHost(method, payload, dest)
			}
			quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.probeFail {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":%d,"limit_window_seconds":604800}}}`, tt.five, tt.week)
			}))
			defer quota.Close()
			cfg.TestProbeBase = quota.URL
			slot := time.Now().In(activationScheduleZone).Truncate(time.Minute)
			open := func() *engine {
				t.Helper()
				e, err := newEngine(cfg, host)
				if err != nil {
					t.Fatal(err)
				}
				e.p.AutoActivate = defaultActivationPolicy()
				e.p.AutoActivate.Enabled = true
				e.p.AutoActivate.IncludeNewAccounts = true
				e.p.AutoActivate.DailyTimes = []string{slot.Format("15:04")}
				e.snapshots[a.ID] = snapshot{Account: a}
				e.snapshots[alias.ID] = snapshot{Account: alias}
				return e
			}
			e := open()
			e.processScheduledSlot(context.Background(), e.p, cfg, []account{a, alias}, slot)
			e.processActivations(context.Background()) // Natural trigger must not duplicate the scheduled call.
			e = open()                                 // No batch checkpoint: simulate restart halfway through the scan.
			e.processScheduledSlot(context.Background(), e.p, cfg, []account{alias, a}, slot)
			f.mu.Lock()
			posts := f.posts
			f.mu.Unlock()
			if posts != tt.wantPosts || e.activationError != "" {
				t.Fatalf("posts=%d (want %d), journal error: %s", posts, tt.wantPosts, e.activationError)
			}
			scheduled := 0
			for _, ev := range e.activationJournal.Events {
				if ev.ScheduledAt == nil {
					continue
				}
				scheduled++
				if ev.State != tt.wantState || !ev.ScheduledAt.Equal(slot) {
					t.Fatalf("unexpected schedule event: %+v", ev)
				}
				if tt.probeFail && !strings.Contains(ev.Reason, "额度核实失败") {
					t.Fatalf("must fail closed on probe error: %+v", ev)
				}
			}
			if scheduled != 1 {
				t.Fatalf("expected one scheduled event for the underlying identity, got %d", scheduled)
			}
		})
	}
}
