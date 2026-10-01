package policy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

// Classify is where a fetch becomes a record. These tests are about the record:
// what was written, in what order, and what was deliberately not written.

func mustClassify(t *testing.T, m *PolicyManager, rawURL string, out Outcome) *Action {
	t.Helper()
	act, err := m.Classify(context.Background(), rawURL, out)
	if err != nil {
		t.Fatalf("Classify(%q) returned an error: %v", rawURL, err)
	}
	if act == nil {
		t.Fatalf("Classify(%q) returned a nil action", rawURL)
	}
	return act
}

// ── success ──────────────────────────────────────────────────────────────────

// TestClassifySuccessCountsThePageAndClearsTheFailures is the whole bookkeeping
// contract for a page that worked.
func TestClassifySuccessCountsThePageAndClearsTheFailures(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/ok"

	// A failure first, so the success has something to clear.
	if _, err := st.RecordFailure(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}

	act := mustClassify(t, m, target, Outcome{StatusCode: 200, ContentType: "text/html"})
	if act.Kind != ActSuccess {
		t.Fatalf("Kind = %v, want success", act.Kind)
	}
	if act.Reason != ReasonFetchOK {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonFetchOK)
	}

	state, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if state.PagesCrawled != 1 {
		t.Errorf("PagesCrawled = %d, want 1", state.PagesCrawled)
	}
	if state.ConsecFailures != 0 {
		t.Errorf("ConsecFailures = %d, want 0; a host that recovered must start its "+
			"next backoff from the first step", state.ConsecFailures)
	}
	if !state.LastSuccess.Equal(now) {
		t.Errorf("LastSuccess = %v, want %v", state.LastSuccess, now)
	}

	// The URL's own retry record is gone, so a URL that works is not carrying its
	// history forward.
	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 0 {
		t.Errorf("attempts = %d after a success, want 0", urlState.Attempts)
	}
}

// TestClassifySuccessDoesNotRetireTheURL is a decision with a reason, and the
// opposite one is easy to take by reflex.
//
// The URL is marked visited when the page is persisted, which is the only point at
// which the fetch has produced anything worth keeping. A page fetched and then lost
// to a database error is a page worth fetching again -- retiring it here would make
// a database outage permanent, silently deleting every page the crawl had fetched
// in the window.
func TestClassifySuccessDoesNotRetireTheURL(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/ok"

	mustClassify(t, m, target, Outcome{StatusCode: 200})

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("a successful fetch marked the URL visited; a page that then fails to " +
			"persist would never be fetched again")
	}
}

// ── permanent ────────────────────────────────────────────────────────────────

// TestClassifyRetiresA404 is the loop this package exists to remove.
//
// A 404 that is not recorded is a 404 that comes back every time anything links to
// it, for ever, and each time it costs a request and a worker slot.
func TestClassifyRetiresA404(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/gone"

	act := mustClassify(t, m, target, Outcome{StatusCode: 404})
	if act.Kind != ActPermanent {
		t.Fatalf("Kind = %v, want permanent", act.Kind)
	}
	if act.Reason != ReasonNotFound {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonNotFound)
	}

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a 404 was not retired; it will be fetched again on every rediscovery")
	}

	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 0 {
		t.Errorf("attempts = %d for a retired URL, want 0: the record is dead weight "+
			"waiting for its TTL", urlState.Attempts)
	}
}

// TestClassifyRetiresBeforeItClears pins the write order, which is not cosmetic.
//
// A URL marked visited whose retry record survived is a dead hash waiting for its
// TTL. A URL whose record was cleared but not retired is a URL that gets fetched
// again. The second failure is the expensive one, so the retiring write goes first
// and a failure of the clearing write cannot undo it.
func TestClassifyRetiresBeforeItClears(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/gone"

	if _, err := st.BumpAttempts(ctx, target); err != nil {
		t.Fatal(err)
	}
	st.FailOn = map[string]error{"ClearURLState": errors.New("redis went away mid-write")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 404})
	if err == nil {
		t.Fatal("expected an error when the decision could not be fully recorded")
	}
	if act != nil {
		t.Fatalf("Action = %+v, want nil: the retiring write landed but the decision "+
			"was only half recorded, so it is not a decision", act)
	}

	st.FailOn = nil
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("the URL was not retired even though the write that clears its record " +
			"failed; a failed clear must not undo the retiring write")
	}
}

// ── backoff ──────────────────────────────────────────────────────────────────

// TestClassifyParksABackoffWithTheComputedDueTime is the mechanism that replaced
// the retry loop.
//
// The old loop slept inside a worker with a fixed count. This parks the URL with a
// due time, so the retry happens when a later crawl promotes it and the delay is
// derived from the host's own failure history.
func TestClassifyParksABackoffWithTheComputedDueTime(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, c := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/flaky"

	act := mustClassify(t, m, target, Outcome{StatusCode: 503})
	if act.Kind != ActBackoff {
		t.Fatalf("Kind = %v, want backoff", act.Kind)
	}
	if act.Reason != ReasonServerError {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonServerError)
	}
	if act.RetryAfter != m.cfg.URLBackoffBase {
		t.Errorf("RetryAfter = %v, want the base %v for a first failure", act.RetryAfter, m.cfg.URLBackoffBase)
	}

	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", urlState.Attempts)
	}

	// The URL must not be due yet, and must not be in the frontier either: a
	// promoted-early retry is a tight loop with extra steps.
	if n, err := st.FrontierLen(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("frontier length = %d, want 0; a parked URL belongs in the delayed set", n)
	}

	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Idle {
		t.Errorf("TakeNext handed out %q before the retry was due", next.URL)
	}

	c.Advance(act.RetryAfter + time.Second)
	next, err = m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Idle || next.URL != target {
		t.Errorf("TakeNext = %+v, want %q once the retry was due", next, target)
	}
}

// TestClassifyGrowsTheDelayWithEachFailure is the per-URL schedule. Doubling is
// what keeps a page that is failing because the site is unwell from being retried
// on every crawl.
func TestClassifyGrowsTheDelayWithEachFailure(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, _, c := atClock(t, now)
	const target = "https://example.com/flaky"

	var last time.Duration
	for i := range 2 {
		act := mustClassify(t, m, target, Outcome{StatusCode: 503})
		if act.RetryAfter <= last {
			t.Fatalf("failure %d: RetryAfter = %v, want more than the previous %v",
				i+1, act.RetryAfter, last)
		}
		last = act.RetryAfter
		c.Advance(act.RetryAfter)
	}
}

// TestClassifyRetiresAURLThatHasRunOutOfAttempts closes the loop. Without it a URL
// whose host is permanently broken is retried for ever, at a growing delay, and the
// growing delay is what hides it: the crawl looks healthy.
func TestClassifyRetiresAURLThatHasRunOutOfAttempts(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, c := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/never-works"

	var last *Action
	for range m.cfg.URLMaxAttempts {
		last = mustClassify(t, m, target, Outcome{StatusCode: 503})
		if last.Kind == ActPermanent {
			break
		}
		c.Advance(last.RetryAfter)
	}
	if last.Kind != ActPermanent {
		t.Fatalf("after %d attempts the kind is %v, want permanent", m.cfg.URLMaxAttempts, last.Kind)
	}
	if last.Reason != ReasonAttemptsExhausted {
		t.Errorf("Reason = %q, want %q: "+
			"a URL given up on for running out of attempts is a different fact from a "+
			"URL that answered 404, and the stats hash has to be able to say so",
			last.Reason, ReasonAttemptsExhausted)
	}

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a URL that ran out of attempts was not retired; it would be retried for ever")
	}
}

// ── markers ──────────────────────────────────────────────────────────────────

// TestClassifyMarksADeadHostOnlyForConnectionFailures is the distinction the whole
// dead-domain design turns on, and it is a distinction between two ways of failing
// that look identical in a log line.
func TestClassifyMarksADeadHostOnlyForConnectionFailures(t *testing.T) {
	tests := []struct {
		name       string
		out        Outcome
		wantDead   bool
		wantReason Reason
	}{
		{
			name:       "a refused dial means the host is gone",
			out:        Outcome{Err: errConnRefused},
			wantDead:   true,
			wantReason: ReasonConnectionRefused,
		},
		{
			name:       "a TLS failure means the host is gone",
			out:        Outcome{Err: errTLSHandshake},
			wantDead:   true,
			wantReason: ReasonTLSError,
		},
		{
			// The one that must never be marked dead. A slow host is not a
			// vanished host, and an hour of skipping is how a site leaves the
			// index because it once took thirty seconds to answer.
			name:       "a timeout must never mark a host dead",
			out:        Outcome{Err: &timeoutError{}},
			wantDead:   false,
			wantReason: ReasonTimeout,
		},
		{
			name:       "a 503 must never mark a host dead",
			out:        Outcome{StatusCode: 503},
			wantDead:   false,
			wantReason: ReasonServerError,
		},
		{
			name:       "rate limiting must never mark a host dead",
			out:        Outcome{StatusCode: 429},
			wantDead:   false,
			wantReason: ReasonRateLimited,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
			m, st, _ := atClock(t, now)
			ctx := context.Background()
			// A hostname, not the subtest name: the marker and the counter are
			// keyed by host, and a name with spaces in it would not survive
			// url.Parse -- every marker assertion below would read "no marker"
			// for the wrong reason and pass or fail by accident.
			host := "probe-" + itoa(len(tc.name)) + ".example"
			target := "https://" + host + "/page"

			act := mustClassify(t, m, target, tc.out)
			if act.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", act.Reason, tc.wantReason)
			}

			markers, err := st.Markers(ctx, host)
			if err != nil {
				t.Fatal(err)
			}
			_, dead := markers[MarkerDead]
			_, cooling := markers[MarkerCooldown]
			if dead != tc.wantDead {
				t.Errorf("dead marker present = %v, want %v (markers: %s)",
					dead, tc.wantDead, markerNames(markers))
			}
			if !tc.wantDead && !cooling {
				t.Errorf("no marker at all was raised for a failing host; markers: %s",
					markerNames(markers))
			}
		})
	}
}

// TestClassifyNeverMarksAHostDeadForATimeout is the single most important case in
// that table, standing on its own so that a refactor that folds it into the dead
// case has to delete a test with a name that says what breaks.
func TestClassifyNeverMarksAHostDeadForATimeout(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "slow.example"
	const target = "https://slow.example/page"

	mustClassify(t, m, target, Outcome{Err: &timeoutError{}})

	markers, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if _, dead := markers[MarkerDead]; dead {
		t.Error("a timeout marked the host dead; its URLs would be answered from Redis " +
			"for an hour because it was slow once")
	}
	if _, cooling := markers[MarkerCooldown]; !cooling {
		t.Errorf("a timeout should cool the host down, got markers: %s", markerNames(markers))
	}
}

// TestTheCooldownRespectsTheRobotsCrawlDelay is acceptance criterion 2.
//
// Crawl-delay was parsed, stored, and then never read -- the defect the whole
// package was written to fix. This is the assertion that it is read.
func TestTheCooldownRespectsTheRobotsCrawlDelay(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "h.example"
	const target = "https://h.example/page"

	if err := st.SaveHostState(ctx, host, HostState{
		Name:            host,
		CrawlDelay:      30 * time.Second,
		MaxPages:        100,
		RobotsFetchedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	mustClassify(t, m, target, Outcome{StatusCode: 503})

	markers, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	ttl, ok := markers[MarkerCooldown]
	if !ok {
		t.Fatalf("no cooldown was raised; markers: %s", markerNames(markers))
	}
	// The delay is a floor, and the failure count still doubles from it -- the
	// point is that the site asked for 30s and the cooldown is not 20s.
	floor := m.cfg.HostCooldown(1, 30*time.Second)
	if ttl != floor {
		t.Errorf("cooldown ttl = %v, want %v (the site's Crawl-delay as the floor)", ttl, floor)
	}
	if ttl < 30*time.Second {
		t.Errorf("cooldown ttl = %v, want at least the 30s the site asked for", ttl)
	}
}

// TestTheDeadMarkerGrowsWithEachConsecutiveFailure is the backoff the dead case
// uses, and it is the host's own count rather than the URL's.
//
// Two URLs on one broken host failing together have to push the host out twice as
// far; a per-URL count would see one failure each and the host would be excluded for
// the first TTL rather than the second.
func TestTheDeadMarkerGrowsWithEachConsecutiveFailure(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "gone.example"

	mustClassify(t, m, "https://gone.example/a", Outcome{Err: &net.DNSError{Err: "no such host"}})
	first, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	firstTTL := first[MarkerDead]

	// The second failure is a different URL on the same host.
	mustClassify(t, m, "https://gone.example/b", Outcome{Err: &net.DNSError{Err: "no such host"}})
	second, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}

	// SetMarker does not extend an existing marker, so the first TTL stands; what
	// grows is the *next* one. Clear it and check that.
	if err := st.ClearMarker(ctx, host, MarkerDead); err != nil {
		t.Fatal(err)
	}
	mustClassify(t, m, "https://gone.example/c", Outcome{Err: &net.DNSError{Err: "no such host"}})
	third, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if third[MarkerDead] <= firstTTL {
		t.Errorf("the dead ttl is %v after three host failures, want more than the %v "+
			"it was after one", third[MarkerDead], firstTTL)
	}
	if second[MarkerDead] != firstTTL {
		t.Errorf("the ttl changed under a live marker: %v then %v; a marker that "+
			"extended itself would keep a host out for ever after enough failures",
			firstTTL, second[MarkerDead])
	}
}

// ── counting ─────────────────────────────────────────────────────────────────

// TestClassifyCountsEveryOutcomeAgainstItsHost is the observability the whole reason
// vocabulary exists for. Without it the stats hash answers "this site is not being
// crawled" with no "why".
func TestClassifyCountsEveryOutcomeAgainstItsHost(t *testing.T) {
	tests := []struct {
		name string
		out  Outcome
		want Reason
	}{
		{"a success", Outcome{StatusCode: 200}, ReasonFetchOK},
		{"a 404", Outcome{StatusCode: 404}, ReasonNotFound},
		{"a 403", Outcome{StatusCode: 403}, ReasonForbidden},
		{"a 503", Outcome{StatusCode: 503}, ReasonServerError},
		{"a refused dial", Outcome{Err: &net.DNSError{Err: "no such host"}}, ReasonDNSFailure},
		{"a timeout", Outcome{Err: &timeoutError{}}, ReasonTimeout},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
			host := "counted.example"
			// The query keeps each subtest on its own URL. They share a host on
			// purpose: the point is that the count lands against the *host*, so
			// giving each its own host would be asserting something else.
			target := "https://" + host + "/p?n=" + strconv.Itoa(len(tc.name))

			act := mustClassify(t, m, target, tc.out)
			if act.Reason != tc.want {
				t.Fatalf("Reason = %q, want %q", act.Reason, tc.want)
			}
			stats := st.Stats(host)
			if stats[tc.want] != 1 {
				t.Errorf("stats[%s] = %d, want 1 (all: %v)", tc.want, stats[tc.want], stats)
			}
		})
	}
}

// TestACountedOutcomeSurvivesAFailedCount is the same reasoning as Admit's: losing
// a count during an outage must not become an error the caller retries, because a
// retry of a recorded decision is a loop.
func TestACountedOutcomeSurvivesAFailedCount(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://counted.example/p"

	st.FailOn = map[string]error{"CountReason": errors.New("redis went away")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 404})
	if err != nil {
		t.Fatalf("a failed count turned into an error: %v", err)
	}
	if act.Kind != ActPermanent {
		t.Errorf("Kind = %v, want permanent; the decision must be applied even when it "+
			"could not be counted", act.Kind)
	}
	st.FailOn = nil
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a failed count stopped the URL being retired")
	}
}

// ── failing closed ───────────────────────────────────────────────────────────

// TestClassifyRefusesToDecideWhenItCannotReadTheAttemptCount.
//
// attempts is the input to the one decision that has no safe default here: read it
// as zero and a URL that has already failed twice gets a fresh budget on every
// attempt, so it is retried for ever; read it as the maximum and a single lost read
// retires a page that was about to work. Neither is a decision this function is
// entitled to make blind, so it makes none.
func TestClassifyRefusesToDecideWhenItCannotReadTheAttemptCount(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/p"

	st.FailOn = map[string]error{"URLState": errors.New("redis went away")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 503})
	if err == nil {
		t.Fatal("expected an error; the attempt count is an input to the decision")
	}
	if act != nil {
		t.Errorf("Action = %+v, want nil: nothing was decided, so there is nothing to return", act)
	}
	if !errors.Is(err, st.FailOn["URLState"]) {
		t.Errorf("error = %v, want it to wrap the state failure so the cause survives", err)
	}
}

// TestClassifyReportsAFailedRecordWrite is the other half, and the reason is
// symmetric with the read.
//
// An outcome that could not be recorded must not be reported as a success, because
// the caller acts on success: it would index a page whose crawl the policy layer
// does not know about, and then fetch and count it again on every rediscovery.
func TestClassifyReportsAFailedRecordWrite(t *testing.T) {
	tests := []struct {
		name   string
		failOn string
		out    Outcome
	}{
		{"a success that cannot be counted", "RecordSuccess", Outcome{StatusCode: 200}},
		{"a success whose url record cannot be cleared", "ClearURLState", Outcome{StatusCode: 200}},
		{"a backoff that cannot be parked", "EnqueueDelayed", Outcome{StatusCode: 503}},
		{"a failure that cannot be counted against the host", "RecordFailure", Outcome{StatusCode: 503}},
		// BumpAttempts is the write that decides how long the next wait is. A
		// backoff computed from a stale attempt count is not a longer wait, it is
		// the same wait again: the URL is parked, so nothing else will notice
		// that the counter never moved, and a host that fails persistently is
		// retried at the first backoff interval for ever.
		{"an attempt that cannot be counted", "BumpAttempts", Outcome{StatusCode: 503}},
		// The cooldown is floored by the site's own Crawl-delay, which means reading
		// host state on the failure path. A read that fails cannot be answered with
		// the zero value: that is "no delay asked for", and a host that asked for
		// thirty seconds would be pushed back in for the base interval instead.
		{"a cooldown whose crawl delay cannot be read", "HostState", Outcome{StatusCode: 503}},
		{"a permanent outcome that cannot be retired", "MarkVisited", Outcome{StatusCode: 404}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
			ctx := context.Background()
			const target = "https://example.com/p"

			st.FailOn = map[string]error{tc.failOn: errors.New("redis went away")}

			act, err := m.Classify(ctx, target, tc.out)
			if err == nil {
				t.Fatalf("expected an error when %s failed", tc.failOn)
			}
			// The Action is withheld, not returned alongside the error. An Action
			// means "this has been applied", and a caller that checked the kind
			// before the error -- one line in the wrong order, in the one function
			// every fetched URL goes through -- would otherwise act on a decision
			// that never reached Redis.
			if act != nil {
				t.Errorf("Action = %+v, want nil alongside the error", act)
			}
		})
	}
}

func TestClassifyRejectsAnEmptyURL(t *testing.T) {
	m, _, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))

	for _, raw := range []string{"", "   "} {
		if _, err := m.Classify(context.Background(), raw, Outcome{StatusCode: 200}); !errors.Is(err, errEmptyURL) {
			t.Errorf("Classify(%q) error = %v, want errEmptyURL", raw, err)
		}
	}
}

// TestClassifyNeedsNoHostToStillDecide covers the URL-shaped edge: a URL that
// parses but has no host still gets a verdict, and the decision is made against the
// URL alone.
func TestClassifyNeedsNoHostToStillDecide(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "mailto:someone@example.com"

	act := mustClassify(t, m, target, Outcome{StatusCode: 404})
	if act.Kind != ActPermanent {
		t.Errorf("Kind = %v, want permanent", act.Kind)
	}
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a hostless URL should still be retirable")
	}
}

// TestTheHostIsLowercasedForTheRecord guards one record per site.
//
// "Example.com" and "example.com" being two host records is two page budgets, two
// stats hashes and two sets of robots rules for one site, and the budget that stops
// being enforced is the one nobody notices.
func TestTheHostIsLowercasedForTheRecord(t *testing.T) {
	for _, raw := range []string{
		"https://EXAMPLE.com/A",
		"https://example.com/A",
	} {
		if got := hostOfURL(raw); got != "example.com" {
			t.Errorf("hostOfURL(%q) = %q, want %q", raw, got, "example.com")
		}
	}
}

func TestHostOfURLReportsNothingForSomethingUnparseable(t *testing.T) {
	for _, raw := range []string{"", "://nope", "https://"} {
		if got := hostOfURL(raw); got != "" {
			t.Errorf("hostOfURL(%q) = %q, want an empty string", raw, got)
		}
	}
}

// timeoutError is defined once in hosts_test.go, where the robots fetcher needs the
// same shape. Reusing it here is deliberate: the classifier is asked to tell a
// timeout from a dead host for the robots fetch and for the page fetch alike, and
// two separately-defined timeout types would let the two paths diverge without any
// test failing.
//
// Asserted against net.Error here so a change to the shape fails loudly rather
// than silently turning these cases into the unrecognised-error branch -- which
// would keep them green while asserting nothing.
var _ net.Error = timeoutError{}
