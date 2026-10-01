package policy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestManager(t *testing.T) (*PolicyManager, *MemoryState) {
	t.Helper()
	st := NewMemoryState()
	return New(DefaultConfig(), st, testLogger()), st
}

// newTestManagerAt is newTestManager with the manager and the state reading the
// same fixed clock. They have to agree: the manager decides when a retry is due,
// and the state stamps the entries it writes, so a manager running ahead of the
// state promotes entries the state believes were written in the future.
func newTestManagerAt(t *testing.T, now time.Time) (*PolicyManager, *MemoryState) {
	t.Helper()
	clock := func() time.Time { return now }
	st := NewMemoryStateAt(clock)
	return New(DefaultConfig(), st, testLogger()).WithClock(clock), st
}

// TestHostGateFailsClosed is the central safety property of the whole package.
//
// Redis holds the only record of what has been crawled. If an unreachable state
// were read as "no state, carry on", the crawl would re-fetch every URL in the
// frontier, ignore every cooldown and every dead marker, and do it at full
// speed against hosts already known to be failing. So an error must never
// produce Allow, and must never produce the terminal Skip either -- Defer is the
// only safe answer, because the URL might be perfectly good once Redis is back.
func TestHostGateFailsClosed(t *testing.T) {
	boom := errors.New("redis is on fire")

	cases := []struct {
		name    string
		breakIt func(*MemoryState)
	}{
		{"markers unreadable", func(s *MemoryState) {}},
		{"host state unreadable", func(s *MemoryState) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestManager(t)
			// Fail only the read the gate makes after markers, so the host-state
			// path is exercised too.
			st.FailWith = boom

			_, verdict, err := m.hostGate(context.Background(), "example.com")
			if err != nil {
				t.Fatalf("hostGate returned an error: %v", err)
			}
			if verdict == nil {
				t.Fatal("hostGate allowed the crawl through with no verdict on a broken state")
			}
			if verdict.Kind == Allow {
				t.Error("hostGate returned Allow with an unreachable state -- this is the " +
					"floodgates-open failure the whole package exists to prevent")
			}
			if verdict.Kind == Skip {
				t.Error("hostGate returned the terminal Skip for a transient state error; " +
					"the URL would be dropped for good over a cache blip")
			}
			if verdict.Kind != Defer {
				t.Errorf("kind = %v, want %v", verdict.Kind, Defer)
			}
			if verdict.Reason != ReasonPolicyUnavailable {
				t.Errorf("reason = %q, want %q", verdict.Reason, ReasonPolicyUnavailable)
			}
		})
	}
}

// TestHostGateOrdering checks that markers are consulted before the state hash,
// because the whole point of a dead marker is to answer without a second lookup.
func TestHostGateOrdering(t *testing.T) {
	t.Run("dead is decided without reading host state", func(t *testing.T) {
		m, st := newTestManager(t)
		ctx := context.Background()

		if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Hour); err != nil {
			t.Fatal(err)
		}
		// The marker read keeps working; only the host-state read behind it is
		// broken. If the gate consulted the state hash first, this would come
		// back as a refusal for the wrong reason -- or, worse, as an allow.
		st.FailOn = map[string]error{"HostState": errors.New("should never be reached")}

		state, verdict, err := m.hostGate(ctx, "example.com")
		if err != nil {
			t.Fatalf("hostGate: %v", err)
		}
		if verdict.Kind != Skip || verdict.Reason != ReasonHostDead {
			t.Errorf("verdict = %v/%q, want Skip/host_dead", verdict.Kind, verdict.Reason)
		}
		if state.Name != "" {
			t.Errorf("host state was read (%+v) despite a dead marker", state)
		}
	})

	t.Run("dead wins over cold and cooldown", func(t *testing.T) {
		m, st := newTestManager(t)
		ctx := context.Background()

		for _, kind := range AllMarkers {
			if err := st.SetMarker(ctx, "example.com", kind, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		_, verdict, _ := m.hostGate(ctx, "example.com")
		if verdict.Reason != ReasonHostDead {
			t.Errorf("reason = %q, want host_dead to win over the others", verdict.Reason)
		}
	})

	t.Run("cold wins over cooldown", func(t *testing.T) {
		m, st := newTestManager(t)
		ctx := context.Background()

		if err := st.SetMarker(ctx, "example.com", MarkerCold, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMarker(ctx, "example.com", MarkerCooldown, time.Minute); err != nil {
			t.Fatal(err)
		}
		_, verdict, _ := m.hostGate(ctx, "example.com")
		if verdict.Kind != Skip || verdict.Reason != ReasonHostCold {
			t.Errorf("verdict = %v/%q, want Skip/host_cold", verdict.Kind, verdict.Reason)
		}
	})
}

// TestHostGateCooldownDefersRatherThanSkips is a distinction that costs a domain
// if it is wrong. A cooldown means the host is answering, just not happily. The
// natural-looking Skip would strand every URL on a host that recovers in ten
// seconds, and a crawl that drops a tenth of a site because it hit one 503 is
// not a crawl.
func TestHostGateCooldownDefersRatherThanSkips(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerCooldown, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	_, verdict, err := m.hostGate(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Kind != Defer {
		t.Errorf("kind = %v, want %v -- a cooled-down host recovers", verdict.Kind, Defer)
	}
	if verdict.Until.IsZero() {
		t.Error("a Defer with no due time is a deadlock: nothing would bring the URL back")
	}
	if d := time.Until(verdict.Until); d > 30*time.Second || d < 29*time.Second {
		t.Errorf("Until is %v from now, want about 30s", d)
	}
}

// TestHostGateDeadReportsWhenItExpires checks that a dead host says when it will
// be probeable again. The parked URL has to come back after the marker, and the
// marker has to be visible to whoever schedules that.
func TestHostGateDeadReportsWhenItExpires(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Both clocks are pinned. The marker deadline is stored by the state and
	// read back by the manager, so leaving either on real time makes the
	// comparison a race against the wall clock rather than a check of anything.
	m = m.WithClock(func() time.Time { return base })
	st.SetClock(func() time.Time { return base })

	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Hour); err != nil {
		t.Fatal(err)
	}

	_, verdict, _ := m.hostGate(ctx, "example.com")
	if got, want := verdict.Until, base.Add(time.Hour); !got.Equal(want) {
		t.Errorf("Until = %v, want %v", got, want)
	}
	if verdict.Reason != ReasonHostDead {
		t.Errorf("reason = %q, want %q", verdict.Reason, ReasonHostDead)
	}
}

// TestHostGateExaminesAMarkerIsExhausted is the property that makes a TTL key
// work at all: once the deadline passes, the host is no longer gated and its
// URLs flow again. This is the implicit probe, and without it a dead domain is
// skipped for the rest of the crawl.
func TestHostGateExaminesAMarkerIsExhausted(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Minute); err != nil {
		t.Fatal(err)
	}

	// Freshly marked: gated, and nothing is read past the marker.
	if _, verdict, _ := m.hostGate(ctx, "example.com"); verdict == nil {
		t.Fatal("a host with a live dead marker was not gated")
	}

	st.Advance(time.Minute)

	// Expired: not gated, and the host flows again. This is the implicit probe.
	st2, verdict, err := m.hostGate(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if verdict != nil {
		t.Errorf("host still gated after the marker expired: %v/%q -- the domain would "+
			"never be probed again", verdict.Kind, verdict.Reason)
	}
	if st2.Name != "example.com" {
		t.Errorf("expected the host record back after expiry, got %+v", st2)
	}
}

// TestSetMarkerDoesNotExtendAnExistingDeadline covers twenty workers deciding a
// host is dead in the same second. With a plain SET each would push the expiry
// out to its own "now plus ttl", so the marker would outlive the backoff
// schedule the policy computed and a recovered host would stay dark for longer
// than the policy says.
func TestSetMarkerDoesNotExtendAnExistingDeadline(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return base })

	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Minute); err != nil {
		t.Fatal(err)
	}
	// A second worker, ten seconds later, asking for a much longer marker
	// because its failure count was higher.
	st.Advance(10 * time.Second)
	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Hour); err != nil {
		t.Fatal(err)
	}

	markers, err := st.Markers(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	ttl := markers[MarkerDead]
	if ttl > 55*time.Second {
		t.Errorf("marker TTL = %v, want about 50s -- SetMarker must not extend an "+
			"existing deadline", ttl)
	}
}

// TestSetMarkerReplacesAnExpiredOne confirms NX does not make a marker
// unraisable: once the old deadline passes, a new failure sets a fresh one.
func TestSetMarkerReplacesAnExpiredOne(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Minute); err != nil {
		t.Fatal(err)
	}
	st.Advance(2 * time.Minute)

	if err := st.SetMarker(ctx, "example.com", MarkerDead, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	markers, _ := st.Markers(ctx, "example.com")
	if _, ok := markers[MarkerDead]; !ok {
		t.Error("a new marker was not raised after the old one expired")
	}
}

func TestSetMarkerWithNonPositiveTTLClears(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerCooldown, time.Minute); err != nil {
		t.Fatal(err)
	}
	// A zero TTL would create a key that vanishes immediately, which reads as
	// "not set" and turns the decision into a silent no-op.
	if err := st.SetMarker(ctx, "example.com", MarkerCooldown, 0); err != nil {
		t.Fatal(err)
	}
	markers, _ := st.Markers(ctx, "example.com")
	if _, ok := markers[MarkerCooldown]; ok {
		t.Error("a zero-TTL marker survived; the cooldown would never be enforced")
	}
}

// TestRefusalIsCounted is the observability payoff. Before this existed, a /login
// or .pdf link vanished at extraction time with no record at all, so "how much
// of this site are we declining and why" had no answer.
func TestRefusalIsCounted(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := m.refuse(ctx, "example.com", "https://example.com/login", Skip, ReasonPathDisallowed, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.refuse(ctx, "example.com", "https://example.com/a.pdf", Skip, ReasonExtensionSkipped, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.refuse(ctx, "example.com", "https://example.com/de", Skip, ReasonLanguageNotEnglish, time.Time{}); err != nil {
		t.Fatal(err)
	}

	stats := st.Stats("example.com")
	want := map[Reason]int{
		ReasonPathDisallowed:     3,
		ReasonExtensionSkipped:   1,
		ReasonLanguageNotEnglish: 1,
	}
	for reason, n := range want {
		if stats[reason] != n {
			t.Errorf("stats[%q] = %d, want %d", reason, stats[reason], n)
		}
	}
	if got := st.StatsTotal("example.com"); got != 5 {
		t.Errorf("total = %d, want 5", got)
	}
}

// TestCountingAFailureNeverBreaksARefusal checks the ordering. The counter is
// observability; losing a count during a partial outage must not turn a correct
// refusal into an error the caller retries, and retrying a refusal is exactly the
// loop this package exists to stop.
func TestCountingAFailureNeverBreaksARefusal(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	st.FailWith = errors.New("counting is down")

	verdict, err := m.refuse(ctx, "example.com", "https://example.com/x", Skip, ReasonPathDisallowed, time.Time{})
	if err != nil {
		t.Fatalf("refuse returned an error because only the counter failed: %v", err)
	}
	if verdict == nil || verdict.Kind != Skip || verdict.Reason != ReasonPathDisallowed {
		t.Errorf("verdict = %+v, want a Skip for the right reason", verdict)
	}
}

// TestHostStateBudgetIsPerWindow is the semantic that a lifetime cap would get
// wrong. If the counter never reset, a host that woke from its cold period would
// find itself instantly over budget again and re-cool without ever being
// crawled -- an infinite cold period that looks like a working limit.
func TestHostStateBudgetIsPerWindow(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	const maxPages = 5
	if err := st.SaveHostState(ctx, "example.com", HostState{
		Name:            "example.com",
		MaxPages:        maxPages,
		WindowStartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxPages; i++ {
		if err := st.RecordSuccess(ctx, "example.com", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := st.HostState(ctx, "example.com")
	if !state.BudgetExhausted(maxPages) {
		t.Fatalf("budget not exhausted after %d pages: %+v", maxPages, state)
	}

	// The window closes and the next one starts.
	if err := st.ResetWindow(ctx, "example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	state, _ = st.HostState(ctx, "example.com")
	if state.BudgetExhausted(maxPages) {
		t.Error("budget still exhausted after the window reset; the host would re-cool " +
			"forever and never be crawled again")
	}
	if state.PagesCrawled != 0 {
		t.Errorf("PagesCrawled = %d after reset, want 0", state.PagesCrawled)
	}
}

// TestBudgetExhaustedPrefersTheLowerLimit covers the "robots may only lower the
// global cap" rule. A host's own limit wins when it is lower, and never when it
// is higher -- a hostile robots.txt must not be able to opt itself into an
// unbounded crawl.
func TestBudgetExhaustedPrefersTheLowerLimit(t *testing.T) {
	tests := []struct {
		name      string
		state     HostState
		globalMax int
		want      bool
	}{
		{"under the global cap", HostState{PagesCrawled: 5}, 10, false},
		{"at the global cap", HostState{PagesCrawled: 10}, 10, true},
		{"a lower host limit wins", HostState{PagesCrawled: 3, MaxPages: 3}, 10, true},
		{"a higher host limit does not raise the cap", HostState{PagesCrawled: 10, MaxPages: 1000}, 10, true},
		// A zero means "not configured", and reading it as an exhausted budget
		// would make every host instantly cold.
		{"no limits at all means no limit", HostState{PagesCrawled: 9999}, 0, false},
		{"a zero host limit falls back to the global", HostState{PagesCrawled: 4, MaxPages: 0}, 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.BudgetExhausted(tc.globalMax); got != tc.want {
				t.Errorf("BudgetExhausted(%d) = %v, want %v", tc.globalMax, got, tc.want)
			}
		})
	}
}

// TestFailuresCountAndReset drives the counter the backoff schedules read.
//
// A success is what clears it, which is the whole point: the count is a count of
// *consecutive* failures, and a host that recovers has to stop paying for the
// failures that made it recover.
func TestFailuresCountAndReset(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	for want := 1; want <= 4; want++ {
		got, err := st.RecordFailure(ctx, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("RecordFailure = %d, want %d", got, want)
		}
	}
	if err := st.RecordSuccess(ctx, "example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	state, _ := st.HostState(ctx, "example.com")
	if state.ConsecFailures != 0 {
		t.Errorf("ConsecFailures = %d after a success, want 0", state.ConsecFailures)
	}
	// And a failure after the success starts from the first step again, rather
	// than resuming at five.
	got, err := st.RecordFailure(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("RecordFailure after a success = %d, want 1", got)
	}
}

// TestURLAttemptsDriveThePerURLSchedule is the plumbing behind the per-page
// backoff: the count classify reads has to be the count that was written.
func TestURLAttemptsDriveThePerURLSchedule(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	const url = "https://example.com/flaky"

	st0, err := st.URLState(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if st0.Attempts != 0 {
		t.Errorf("a url with no record reported %d attempts, want 0", st0.Attempts)
	}

	for want := 1; want <= 3; want++ {
		got, err := st.BumpAttempts(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("BumpAttempts = %d, want %d", got, want)
		}
	}

	if err := st.ClearURLState(ctx, url); err != nil {
		t.Fatal(err)
	}
	st0, _ = st.URLState(ctx, url)
	if st0.Attempts != 0 {
		t.Errorf("Attempts = %d after clear, want 0", st0.Attempts)
	}
}

// TestVisitedIsTerminal is what makes a Skip stick. A visited URL must never be
// handed out again, and the whole point of skipping a dead host's URLs is that
// the frontier stops producing them.
func TestVisitedIsTerminal(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	const url = "https://example.com/dead-page"

	if err := st.MarkVisited(ctx, url); err != nil {
		t.Fatal(err)
	}
	got, err := st.IsVisited(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("a marked url is not reported visited")
	}
}

// TestMarkVisitedDropsEmptyURLs guards the set's integrity. An empty member
// would match a URL that failed to parse, which is exactly the case the visited
// set is supposed to exclude.
func TestMarkVisitedDropsEmptyURLs(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	if err := st.MarkVisited(ctx, "", "https://example.com/ok", ""); err != nil {
		t.Fatal(err)
	}
	if len(st.Visited()) != 1 {
		t.Errorf("visited = %v, want only the one real url", st.Visited())
	}
}

// TestEnqueuePriorityIsAnInlinkCount documents the frontier's scoring, because
// ZPOPMAX takes the *highest* score and the sign of the increment is easy to get
// backwards: a URL linked from many places should come out first.
func TestEnqueuePriorityIsAnInlinkCount(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	urls := []string{"https://example.com/rare", "https://example.com/common", "https://example.com/mid"}
	for _, u := range urls {
		if err := st.Enqueue(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	// "common" is linked from three pages, the others from one each.
	if err := st.Enqueue(ctx, "https://example.com/common", "https://example.com/common"); err != nil {
		t.Fatal(err)
	}

	order := st.FrontierOrder()
	if len(order) != 3 {
		t.Fatalf("frontier = %v, want 3 urls", order)
	}
	if order[0] != "https://example.com/common" {
		t.Errorf("frontier order = %v, want the most-linked url first", order)
	}
}

// TestWithClockDoesNotMutateTheOriginal keeps the test seam from leaking into
// production behaviour: a copy is returned, so a manager that has been given a
// fixed clock in one test cannot surprise another.
func TestWithClockDoesNotMutateTheOriginal(t *testing.T) {
	m, _ := newTestManager(t)
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	pinned := m.WithClock(func() time.Time { return fixed })

	if !pinned.Now().Equal(fixed) {
		t.Errorf("pinned clock = %v, want %v", pinned.Now(), fixed)
	}
	if m.Now().Equal(fixed) {
		t.Error("WithClock mutated the receiver; the manager's own clock is no longer real time")
	}
	if pinned.WithClock(nil).Now().Equal(fixed) != true {
		t.Error("a nil clock should leave the existing one in place")
	}
}

// TestLogIsOptional keeps New usable without a configured logger, since a nil
// slog.Logger would otherwise panic on the first refusal.
func TestLogIsOptional(t *testing.T) {
	st := NewMemoryState()
	m := New(DefaultConfig(), st, nil)
	if m.log == nil {
		t.Fatal("New left the logger nil")
	}
	if _, err := m.refuse(context.Background(), "example.com", "https://example.com/x", Skip, ReasonVisited, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStateRejectsUseAfterClose(t *testing.T) {
	st := NewMemoryState()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Markers(context.Background(), "example.com"); err == nil {
		t.Error("a closed state returned no error; callers would read it as an empty host")
	}
}
