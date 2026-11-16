package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func activationCoreFixture(t *testing.T) (*engine, activationPolicy, account, string, time.Time) {
	t.Helper()
	e := testEngine(t)
	p := defaultActivationPolicy()
	p.Enabled = true
	p.Accounts["a.json"] = true
	a := account{ID: "a", Name: "a.json", AuthIndex: "auth-a", Provider: "codex"}
	return e, p, a, activationHash("test-account"), time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
}
func activationObserveTest(e *engine, p activationPolicy, a account, id string, at time.Time, b ...bucket) {
	e.observeActivationLocked(id, snapshot{Account: a, Buckets: b, CheckedAt: at}, p, at)
}
func activationWindowTest(seconds int64, remaining float64, at time.Time) bucket {
	return bucket{ID: "primary", WindowSeconds: seconds, RemainingPercent: remaining, ResetAt: at}
}
func activationPendingTest(t *testing.T, e *engine, id string) activationOperation {
	t.Helper()
	s := e.activationJournal.Identities[id]
	if s == nil || s.Pending == "" {
		t.Fatal("expected pending activation")
	}
	return e.activationJournal.Operations[s.Pending]
}
func activationFinishTest(e *engine, id, state string, at time.Time) {
	s := e.activationJournal.Identities[id]
	op := e.activationJournal.Operations[s.Pending]
	op.State = state
	op.At = at
	e.activationJournal.Operations[op.ID] = op
	settleActivationWindows(s, op)
	s.Pending = ""
	for k, w := range s.Windows {
		w.Handled = true
		s.Windows[k] = w
	}
}

func TestActivationNaturalRefreshWithFutureResetTime(t *testing.T) {
	for _, seconds := range []int64{18000, 604800} {
		t.Run(fmtInt(seconds), func(t *testing.T) {
			e, p, a, id, now := activationCoreFixture(t)
			deadline := now.Add(4 * time.Second)
			b := activationWindowTest(seconds, 0, deadline)
			activationObserveTest(e, p, a, id, now, b)
			at := deadline.Add(time.Second)
			b.RemainingPercent = 100
			b.ResetAt = at.Add(time.Duration(seconds) * time.Second)
			activationObserveTest(e, p, a, id, at, b)
			op := activationPendingTest(t, e, id)
			if op.Cycles[activationKind(b)] != "natural:"+fmtInt(deadline.Unix()) {
				t.Fatalf("future reset hid natural refresh: %+v", op)
			}
			b.ResetAt = b.ResetAt.Add(time.Minute)
			activationObserveTest(e, p, a, id, at.Add(time.Minute), b)
			if activationPendingTest(t, e, id).ID != op.ID {
				t.Fatal("preflight drift must preserve pending cycle")
			}
		})
	}
}

func TestActivationManualRefreshWithChangedFutureResetTime(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 20, now.Add(time.Hour))
	activationObserveTest(e, p, a, id, now, b)
	b.RemainingPercent = 100
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("rounding to full with unchanged active timer is not a reset")
	}
	// Consumption evidence must survive intermediate snapshots rounded to full.
	b.ResetAt = now.Add(5 * time.Hour)
	activationObserveTest(e, p, a, id, now.Add(3*time.Second), b)
	op := activationPendingTest(t, e, id)
	if op.Cycles["five_hour"] != "observed-reset:"+fmtInt(now.Add(time.Hour).Unix()) {
		t.Fatalf("future reset hid manual refresh: %+v", op)
	}
}

func TestActivationInitialFullFutureDriftAndTerminalDedup(t *testing.T) {
	for _, state := range []string{"succeeded", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			e, p, a, id, now := activationCoreFixture(t)
			b := activationWindowTest(18000, 100, now.Add(5*time.Hour))
			activationObserveTest(e, p, a, id, now, b)
			if e.activationJournal.Identities[id].Pending != "" {
				t.Fatal("first future timer alone is insufficient")
			}
			// Compare accumulated drift against the anchor, not only the last query.
			for i := 1; i <= 3; i++ {
				b.ResetAt = now.Add(5*time.Hour + time.Duration(i*2)*time.Second)
				activationObserveTest(e, p, a, id, now.Add(time.Duration(i*2)*time.Second), b)
			}
			op := activationPendingTest(t, e, id)
			activationFinishTest(e, id, state, now.Add(7*time.Second))
			if err := writeActivationJournal(e.cfg.StateDir, e.activationJournal); err != nil {
				t.Fatal(err)
			}
			j, err := loadActivationJournal(e.cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			e.activationJournal = j
			for i := 1; i <= 20; i++ {
				at := now.Add(time.Duration(i) * time.Minute)
				b.ResetAt = at.Add(5 * time.Hour)
				activationObserveTest(e, p, a, id, at, b)
				if e.activationJournal.Identities[id].Pending != "" {
					t.Fatal("placeholder drift repeated sent request")
				}
			}
			w := e.activationJournal.Identities[id].Windows["five_hour"]
			if len(e.activationJournal.Operations) != 1 || e.activationJournal.Operations[op.ID].Confirmed {
				t.Fatal("future placeholder must neither repeat nor confirm the call")
			}
			// Even tiny successful calls may round to 100%; the next real boundary
			// must remain scheduled without requiring visible percentage consumption.
			at := w.ActiveUntil.Add(time.Second)
			b.ResetAt = at.Add(5 * time.Hour)
			activationObserveTest(e, p, a, id, at, b)
			next := activationPendingTest(t, e, id)
			if next.ID == op.ID || next.Cycles["five_hour"] != "natural:"+fmtInt(w.ActiveUntil.Unix()) {
				t.Fatal("rounded full usage lost the next natural boundary")
			}
		})
	}
}

func TestActivationFixedFullFutureAndJitterAreNotIdleEvidence(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, now.Add(time.Hour))
	activationObserveTest(e, p, a, id, now, b)
	for i := 1; i < 10; i++ {
		b.ResetAt = now.Add(time.Hour + time.Duration(i%3)*time.Second)
		activationObserveTest(e, p, a, id, now.Add(time.Duration(i)*time.Minute), b)
		if e.activationJournal.Identities[id].Pending != "" {
			t.Fatal("stable full timer is already active")
		}
	}
}

func TestActivationResetSignalAcceptsFullFutureTimer(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.Natural = false
	e.markActivationReset(id, "future-card", now)
	b := activationWindowTest(604800, 100, now.Add(7*24*time.Hour))
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	if activationPendingTest(t, e, id).Cycles["weekly"] != "reset:future-card" {
		t.Fatal("explicit reset with future timer was ignored")
	}
}

func TestActivationSentRequestStartsUnselectedFullWindow(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.FiveHour = false
	b := activationWindowTest(18000, 100, now.Add(5*time.Hour))
	w := activationWindowTest(604800, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b, w)
	activationFinishTest(e, id, "succeeded", now.Add(time.Second))
	p.FiveHour = true
	b.ResetAt = now.Add(5*time.Hour + time.Minute)
	w.ResetAt = now.Add(7 * 24 * time.Hour)
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b, w)
	b.ResetAt = b.ResetAt.Add(time.Minute)
	activationObserveTest(e, p, a, id, now.Add(2*time.Minute), b, w)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("one request starts all full windows")
	}
}

func TestActivationFutureRefreshRespectsSourceSwitches(t *testing.T) {
	for _, natural := range []bool{false, true} {
		e, p, a, id, now := activationCoreFixture(t)
		deadline := now.Add(time.Hour)
		b := activationWindowTest(18000, 20, deadline)
		activationObserveTest(e, p, a, id, now, b)
		p.Natural = !natural
		p.Reset = natural
		at := now.Add(time.Minute)
		if natural {
			at = deadline.Add(time.Second)
		}
		b.RemainingPercent = 100
		b.ResetAt = at.Add(5 * time.Hour)
		activationObserveTest(e, p, a, id, at, b)
		if e.activationJournal.Identities[id].Pending != "" {
			t.Fatal("disabled source must not be relabeled as the other source")
		}
	}
}

func TestActivationFutureDriftDoesNotChangeNoSendRetry(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, now.Add(5*time.Hour))
	activationObserveTest(e, p, a, id, now, b)
	b.ResetAt = b.ResetAt.Add(time.Minute)
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b)
	op := activationPendingTest(t, e, id)
	op.State, op.NoSend, op.RetryAt = "failed", true, now.Add(6*time.Minute)
	e.activationJournal.Operations[op.ID] = op
	b.ResetAt = b.ResetAt.Add(time.Minute)
	activationObserveTest(e, p, a, id, now.Add(2*time.Minute), b)
	if retry := activationPendingTest(t, e, id); retry.ID != op.ID || !retry.RetryAt.Equal(op.RetryAt) {
		t.Fatal("drift bypassed preflight backoff")
	}
}

func TestActivationInitialIdleMergesWindowsAndNeverRepeats(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b5 := activationWindowTest(18000, 100, time.Time{})
	bw := activationWindowTest(604800, 100, time.Time{})
	bw.ID = "secondary"
	activationObserveTest(e, p, a, id, now, b5, bw)
	op := activationPendingTest(t, e, id)
	if len(op.Windows) != 2 || op.Windows[0] != "five_hour" || op.Windows[1] != "weekly" {
		t.Fatalf("windows not merged: %+v", op)
	}
	activationFinishTest(e, id, "succeeded", now.Add(time.Second))
	for i := 2; i < 20; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		b5.ResetAt = at
		bw.ResetAt = at
		activationObserveTest(e, p, a, id, at, b5, bw)
		if e.activationJournal.Identities[id].Pending != "" {
			t.Fatal("idle relative reset clock drift must not cause another call")
		}
	}
}

func TestActivationNaturalFirstObservedActiveCycle(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	deadline := now.Add(4 * time.Second)
	e.p.AutoActivate = p
	b := activationWindowTest(18000, 43, deadline)
	activationObserveTest(e, p, a, id, now, b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("active cycle must not receive an extra call")
	}
	if d := e.activationDelay(now); d > 5*time.Second {
		t.Fatalf("known reset must wake before full quota poll: %s", d)
	}
	b.RemainingPercent = 100
	activationObserveTest(e, p, a, id, deadline.Add(time.Second), b)
	op := activationPendingTest(t, e, id)
	if op.Cycles["five_hour"] != "natural:"+fmtInt(deadline.Unix()) {
		t.Fatalf("not natural: %+v", op)
	}
	activationFinishTest(e, id, "succeeded", deadline.Add(2*time.Second))
	activationObserveTest(e, p, a, id, deadline.Add(time.Minute), b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("same expired boundary was reused")
	}
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }

func TestActivationOnlyWeeklyPrimaryIsSupported(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(604800, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	op := activationPendingTest(t, e, id)
	if len(op.Windows) != 1 || op.Windows[0] != "weekly" {
		t.Fatalf("weekly primary must use duration: %+v", op.Windows)
	}
	p.Weekly = false
	e.queueActivationLocked(id, a, p, now.Add(time.Second))
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("window selection switch must cancel pending task")
	}
}

func TestActivationManualResetBeforeNaturalDeadline(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 40, now.Add(time.Hour))
	activationObserveTest(e, p, a, id, now, b)
	b.RemainingPercent = 100
	b.ResetAt = now.Add(-time.Second)
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	op := activationPendingTest(t, e, id)
	if len(op.Cycles["five_hour"]) < 15 || op.Cycles["five_hour"][:15] != "observed-reset:" {
		t.Fatalf("manual reset not identified: %+v", op)
	}
	p.Reset = false
	e.queueActivationLocked(id, a, p, now.Add(2*time.Second))
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("disabling reset source must cancel old pending operation")
	}
}

func TestActivationResetSignalWithoutBaselineAndNaturalDisabled(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	p.Natural = false
	e.markActivationReset(id, "redemption-1", now)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	op := activationPendingTest(t, e, id)
	if op.Cycles["five_hour"] != "reset:redemption-1" {
		t.Fatalf("reset-only signal lost: %+v", op)
	}
	activationFinishTest(e, id, "succeeded", now.Add(2*time.Second))
	e.markActivationReset(id, "redemption-1", now)
	activationObserveTest(e, p, a, id, now.Add(3*time.Second), b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("same successful redemption must never activate again")
	}
}

func TestActivationBusinessRequestWinsBeforeAutomaticSend(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	idle := activationPendingTest(t, e, id)
	b.RemainingPercent = 99.999
	b.ResetAt = now.Add(5 * time.Hour)
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	if e.activationJournal.Identities[id].Pending != "" || e.activationJournal.Operations[idle.ID].State != "skipped" {
		t.Fatal("business traffic should settle pending activation")
	}
}

func TestActivationRelativeFutureJitterKeepsBoundaryStable(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	deadline := now.Add(time.Hour)
	b := activationWindowTest(18000, 50, deadline)
	activationObserveTest(e, p, a, id, now, b)
	b.ResetAt = deadline.Add(2 * time.Second)
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b)
	w := e.activationJournal.Identities[id].Windows["five_hour"]
	if !w.ActiveUntil.Equal(deadline) {
		t.Fatal("relative clock rounding should not move stable reset anchor")
	}
}

func TestActivationCachedActiveIdleOscillationReusesResetCycle(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	active := activationWindowTest(18000, 40, now.Add(time.Hour))
	idle := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, active)
	activationObserveTest(e, p, a, id, now.Add(time.Second), idle)
	op := activationPendingTest(t, e, id)
	activationFinishTest(e, id, "succeeded", now.Add(2*time.Second))
	for i := 3; i < 10; i += 2 {
		activationObserveTest(e, p, a, id, now.Add(time.Duration(i)*time.Second), active)
		activationObserveTest(e, p, a, id, now.Add(time.Duration(i+1)*time.Second), idle)
		if e.activationJournal.Identities[id].Pending != "" {
			t.Fatal("cached repeated transition must not consume another activation")
		}
	}
	if len(e.activationJournal.Operations) != 1 || e.activationJournal.Operations[op.ID].State != "succeeded" {
		t.Fatal("one known active cycle must have only one reset activation")
	}
}

func TestActivationManualResetRetiresPreviousNaturalBoundary(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	oldDeadline := now.Add(time.Hour)
	active := activationWindowTest(18000, 40, oldDeadline)
	idle := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, active)
	activationObserveTest(e, p, a, id, now.Add(time.Second), idle)
	_ = activationPendingTest(t, e, id)
	activationFinishTest(e, id, "succeeded", now.Add(2*time.Second))
	activationObserveTest(e, p, a, id, oldDeadline.Add(time.Minute), idle)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("old pre-reset deadline must not open another natural activation")
	}
}

func TestActivationNaturalSourceDisabledCancelsQueuedWork(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	op := activationPendingTest(t, e, id)
	p.Natural = false
	activationObserveTest(e, p, a, id, now.Add(time.Second), b)
	if e.activationJournal.Identities[id].Pending != "" || e.activationJournal.Operations[op.ID].State != "skipped" {
		t.Fatal("source switch must prevent old queued request")
	}
}

func TestActivationPreflightFailureKeepsSafeRetryAndTerminalUncertainDoesNot(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	op := activationPendingTest(t, e, id)
	op.State = "failed"
	op.NoSend = true
	op.RetryAt = now.Add(5 * time.Minute)
	e.activationJournal.Operations[op.ID] = op
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b)
	if activationPendingTest(t, e, id).ID != op.ID {
		t.Fatal("preflight failure must remain recoverable without opening a new cycle")
	}
	op.State = "uncertain"
	op.NoSend = false
	e.activationJournal.Operations[op.ID] = op
	activationObserveTest(e, p, a, id, now.Add(2*time.Minute), b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("uncertain sent request must never become retryable")
	}
}

func TestActivationUnknownAndExhaustedQuotaNeverReady(t *testing.T) {
	e, p, a, _, now := activationCoreFixture(t)
	_ = p
	for _, b := range []bucket{activationWindowTest(18000, -1, now), activationWindowTest(18000, 0, now), activationWindowTest(3600, 100, now)} {
		s := snapshot{Account: a, CheckedAt: now, Buckets: []bucket{activationWindowTest(604800, 100, time.Time{}), b}}
		if activationQuotaReady(s, e.p, now) == nil {
			t.Fatalf("unsafe quota accepted: %+v", b)
		}
	}
	s := snapshot{Account: a, CheckedAt: now, Buckets: []bucket{activationWindowTest(604800, 100, time.Time{})}}
	if activationQuotaReady(s, e.p, now) != nil {
		t.Fatal("known weekly-only plan should be ready")
	}
	s.Error = "offline"
	if activationQuotaReady(s, e.p, now) == nil {
		t.Fatal("failed query must not reuse old successful snapshot for automatic calls")
	}
}

func TestActivationProtectedAccountIsBlockedIndependentlyOfMode(t *testing.T) {
	e, _, a, _, now := activationCoreFixture(t)
	e.p.Mode = "observe"
	e.p.Rules = []rule{{Account: a.Name, Enabled: true, MinRemainingPercent: 20}}
	e.snapshots[a.ID] = snapshot{Account: a, CheckedAt: now, Buckets: []bucket{activationWindowTest(18000, 22, now.Add(time.Hour)), activationWindowTest(604800, 100, time.Time{})}}
	if !e.evaluate(a, now).Protected {
		t.Fatal("automatic activation must respect protection even in observation mode")
	}
}

func TestActivationJournalRestartMarksInflightUncertain(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	op := activationPendingTest(t, e, id)
	op.State = "sending"
	e.activationJournal.Operations[op.ID] = op
	if err := writeActivationJournal(e.cfg.StateDir, e.activationJournal); err != nil {
		t.Fatal(err)
	}
	j, err := loadActivationJournal(e.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	e.activationJournal = j
	if j.Operations[op.ID].State != "uncertain" {
		t.Fatal("inflight request must never be automatically resent")
	}
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b)
	if e.activationJournal.Identities[id].Pending != "" {
		t.Fatal("restart scheduled uncertain request again")
	}
	if info, err := os.Stat(filepath.Join(e.cfg.StateDir, "activation-journal.json")); err != nil || info.Size() == 0 {
		t.Fatal("missing durable journal")
	}
}

func TestActivationJournalCorruptionFailsClosed(t *testing.T) {
	e, _, _, _, _ := activationCoreFixture(t)
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "activation-journal.json"), []byte(`{"version":1,"identities":{},"operations":{"bad":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	e.initActivation()
	if e.activationError == "" {
		t.Fatal("corrupt journal must pause automatic requests")
	}
}

func TestActivationAccountAliasesDeduplicateByUpstreamIdentity(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	first := activationPendingTest(t, e, id)
	alias := a
	alias.ID = "alias"
	alias.Name = "alias.json"
	activationObserveTest(e, p, alias, id, now.Add(time.Second), b)
	if len(e.activationJournal.Operations) != 1 || activationPendingTest(t, e, id).ID != first.ID {
		t.Fatal("aliases must share one activation")
	}
	host := func(_ string, _ any, out any) error {
		raw, _ := json.Marshal(map[string]any{"json": map[string]any{"account_id": "same-real-account"}})
		return json.Unmarshal(raw, out)
	}
	left, err := activationAccountIdentity(host, a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := activationAccountIdentity(host, alias)
	if err != nil || left != right {
		t.Fatal("identity should use real account_id")
	}
}

func TestActivationLegacyPolicyDefaultsDisabledAndModelInheritance(t *testing.T) {
	e, _, a, _, _ := activationCoreFixture(t)
	raw := `{"revision":1,"mode":"observe","poll_seconds":60,"max_age_seconds":180,"timeout_seconds":15,"safety_percent":3,"resume_gap_percent":5,"unknown_action":"block","strategy":"round-robin","rules":[]}`
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "policy.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(e.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.AutoActivate.Enabled || p.AutoActivate.IncludeNewAccounts || !p.AutoActivate.Natural || !p.AutoActivate.Reset || !p.AutoActivate.FiveHour || !p.AutoActivate.Weekly {
		t.Fatalf("legacy defaults unsafe: %+v", p.AutoActivate)
	}
	p.AutoActivate.Model = "gpt-mini"
	p.AutoActivate.AccountModels[a.Name] = ""
	if activationPreferredModel(p.AutoActivate, a) != "gpt-mini" {
		t.Fatal("empty account model must inherit global model")
	}
}

func TestActivationSuccessfulEventClockCanBeConfirmedLater(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	op := activationPendingTest(t, e, id)
	activationFinishTest(e, id, "succeeded", now.Add(time.Second))
	e.activationJournal.Events = append(e.activationJournal.Events, e.activationJournal.Operations[op.ID].activationEvent)
	b.ResetAt = now.Add(5 * time.Hour)
	b.RemainingPercent = 99.999
	activationObserveTest(e, p, a, id, now.Add(time.Minute), b)
	if !e.activationJournal.Operations[op.ID].Confirmed || !e.activationJournal.Events[0].Confirmed {
		t.Fatal("later successful clock query must confirm the original event")
	}
}

func TestActivationStatusRestoresUncertainAndClearsRecoveredQueryError(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	b := activationWindowTest(18000, 100, time.Time{})
	activationObserveTest(e, p, a, id, now, b)
	activationFinishTest(e, id, "uncertain", now.Add(time.Second))
	e.activationStatuses = map[string]activationAccountStatus{}
	v, _, _ := e.activationView(a, p)
	if v.State != "uncertain" {
		t.Fatalf("restart hid uncertain result: %+v", v)
	}
	e.setActivationStatus(a, "blocked", "temporary query failure", now.Add(time.Minute))
	activationObserveTest(e, p, a, id, now.Add(2*time.Second), b)
	v, _, _ = e.activationView(a, p)
	if v.State != "uncertain" {
		t.Fatalf("healthy query retained stale blocked state: %+v", v)
	}
}

func TestActivationStatusReportsUpcomingNaturalBoundary(t *testing.T) {
	e, p, a, id, now := activationCoreFixture(t)
	deadline := now.Add(time.Hour)
	b := activationWindowTest(604800, 40, deadline)
	activationObserveTest(e, p, a, id, now, b)
	v, _, _ := e.activationView(a, p)
	if !v.NextAt.Equal(deadline.Add(time.Second)) {
		t.Fatalf("expected cached reset wake time, got %s", v.NextAt)
	}
}
