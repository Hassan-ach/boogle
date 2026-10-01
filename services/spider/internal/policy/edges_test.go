package policy

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRobotsURLForRejectsWhatIsNotAHostname is the last check before a network
// call, and it is the one that decides *where* the call goes.
//
// The host arrives from a URL, so in principle it is always a hostname. In
// practice a URL with a userinfo section, an embedded path, a query or a space
// produces something called a host that is none of those, and
// "https://user@evil.example/x/robots.txt" is a request to a host nobody chose,
// with credentials attached. Every one of these is refused rather than sanitised,
// because a sanitised host is a different host and nobody would know.
func TestRobotsURLForRejectsWhatIsNotAHostname(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"example.com/path",
		"example.com?x=1",
		"example.com#frag",
		"user@example.com",
		"exa mple.com",
		"example.com:80/x",
		"http://example.com",
		"example.com\n/../other",
	}
	for _, host := range bad {
		if got, err := robotsURLFor(host); err == nil {
			t.Errorf("robotsURLFor(%q) = %q, want an error", host, got)
		}
	}
}

// TestRobotsURLForKeepsWhatItShould covers the forms that are legitimate and must
// survive, because dropping them would mean never reading a rules file.
func TestRobotsURLForKeepsWhatItShould(t *testing.T) {
	cases := []struct{ host, want string }{
		{"example.com", "https://example.com/robots.txt"},
		// A port is part of the host. Dropping it fetches a different virtual host
		// on the same address, which on a shared host answers for someone else --
		// or for nothing at all.
		{"example.com:8080", "https://example.com:8080/robots.txt"},
		{"example.com:443", "https://example.com:443/robots.txt"},
		// Uppercase is a hostname; case-folding it is correct and keeps the key
		// agreeing with the one Admit derives.
		{"EXAMPLE.com", "https://example.com/robots.txt"},
		{"xn--bcher-kva.example", "https://xn--bcher-kva.example/robots.txt"},
		{"localhost", "https://localhost/robots.txt"},
		{"192.0.2.1", "https://192.0.2.1/robots.txt"},
	}

	for _, tc := range cases {
		got, err := robotsURLFor(tc.host)
		if err != nil {
			t.Errorf("robotsURLFor(%q) = %v, want %q", tc.host, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("robotsURLFor(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestAgentMatchesIsASubstringTest documents what the standard's agent matching
// actually is, because a prefix or equality test is the usual mistake.
//
// The standard defines the match as a case-insensitive substring of the crawler's
// product token, which is why "BoogleBot" matches a stanza for "booglebot" and one
// for "BoogleBot/1.0". Equality would miss the second, which is a common spelling
// once a version is appended.
func TestAgentMatchesIsASubstringTest(t *testing.T) {
	cases := []struct {
		token, agent string
		want         bool
	}{
		{"BoogleBot", "BoogleBot", true},
		{"booglebot", "BoogleBot/1.0", true},
		{"BoogleBot/1.0", "BoogleBot/1.0", true},
		{"Boogle", "BoogleBot", true},
		{"Googlebot", "BoogleBot", false},
		{"bot", "BoogleBot", true},
		// The empty cases matter: a blank stanza name must not match everything,
		// which would apply a nameless group to every crawler on the web.
		{"", "BoogleBot", false},
		{"BoogleBot", "", false},
		{"", "", false},
	}

	for _, tc := range cases {
		if got := agentMatches(tc.token, tc.agent); got != tc.want {
			t.Errorf("agentMatches(%q, %q) = %v, want %v", tc.token, tc.agent, got, tc.want)
		}
	}
}

// TestParseRobotsSkipsANamelessGroup covers the malformed case that a nameless
// stanza creates.
//
// "User-agent:" with nothing after it is not a wildcard. Treating it as one would
// apply that group's rules to every crawler, including us, which is a site
// accidentally publishing rules for an agent that does not exist.
func TestParseRobotsSkipsANamelessGroup(t *testing.T) {
	got := parseRobots("User-agent:\nDisallow: /\n\nUser-agent: *\nDisallow: /x\n", "BoogleBot")
	if len(got.Disallow) == 0 {
		t.Fatal("nothing was parsed")
	}
	if equalStrings(got.Disallow, []string{"/"}) {
		t.Error("a nameless group was applied; a blank agent is not a wildcard")
	}
}

// TestParseRobotsOnlyAppliesEachGroupOnce covers the duplicate-agent case.
//
// A robots.txt naming the same agent in two separate stanzas is malformed, but it
// happens. Both stanzas' rules are legitimately ours; applying the first twice
// would double-count nothing useful and, more importantly, means the dedupe path
// rather than the merge path is what is being exercised.
func TestParseRobotsOnlyAppliesEachGroupOnce(t *testing.T) {
	got := parseRobots(
		"User-agent: BoogleBot\nDisallow: /a\n\nUser-agent: BoogleBot\nDisallow: /b\n",
		"BoogleBot")
	if want := []string{"/a", "/b"}; !equalStrings(got.Disallow, want) {
		t.Errorf("Disallow = %#v, want %#v", got.Disallow, want)
	}
}

// TestDropsQueryParam covers the last of the table-driven rules, and the one that
// is not a rule at all.
//
// These parameters are dropped rather than refused, because a parameter that only
// varies the view -- sort order, page number, a search term -- makes one document
// into an unbounded number of near-identical keys, each competing for the same
// words. That is a statement about what URL this is, so utils.CanonicalizeUrl
// applies it and the crawler never sees those parameters again. Which is also why
// ReasonQueryParamFiltered has no producer today: by the time Admit runs, a URL
// that had ?utm_source=news is indistinguishable from one that never did.
func TestDropsQueryParam(t *testing.T) {
	r := DefaultRules()

	for _, q := range []string{"sort", "page", "filter", "q", "search"} {
		if !r.DropsQueryParam(q) {
			t.Errorf("DropsQueryParam(%q) = false, want true", q)
		}
	}

	for _, q := range []string{
		"", "id", "lang", "v",
		// Near misses. "page_size" is a real parameter on half the sites that have
		// pagination, and matching it as a prefix would merge page 1 of two
		// different listings.
		"page_size", "sort_order", "query", "searching",
		// Tracking parameters are not in this table. They belong to the same
		// problem and a different owner; see the field's comment.
		"utm_source", "fbclid",
	} {
		if r.DropsQueryParam(q) {
			t.Errorf("DropsQueryParam(%q) = true, want false", q)
		}
	}
}

// TestDropsQueryParamOnAnEmptyTable says what an emptied table means, so that a
// caller who clears it knows it cleared it.
//
// Unlike the path and extension rules, an empty query-parameter list is harmless:
// there is no entry that would match every URL the way an empty path rule would.
// That asymmetry is the whole reason the other tables need an empty-entry guard and
// this one does not.
func TestDropsQueryParamOnAnEmptyTable(t *testing.T) {
	var empty RuleSet
	for _, q := range []string{"", "sort", "anything"} {
		if empty.DropsQueryParam(q) {
			t.Errorf("an empty table dropped %q", q)
		}
	}
}

// TestRobotsStaleTreatsANeverReadHostAsStale pins the zero point of the TTL, which
// is the direction that matters.
//
// A host with no fetch timestamp has no rules, and "no rules" is not the same as
// "old rules". Reading the zero time as fresh would let a host whose EnsureHost
// never succeeded be treated as resolved with an empty rule set -- that is, as a
// host that permits everything because it never answered.
func TestRobotsStaleTreatsANeverReadHostAsStale(t *testing.T) {
	m, _ := newTestManager(t)

	if !m.robotsStale(HostState{Name: "h.example"}) {
		t.Error("a host that was never read was reported as fresh; its rules " +
			"would be treated as an empty set that permits everything")
	}

	// Read now, with the shipped TTL: fresh.
	if m.robotsStale(HostState{Name: "h.example", RobotsFetchedAt: m.now()}) {
		t.Error("a robots.txt read a moment ago was reported as stale")
	}
}

// TestMemoryStateReportsItsOwnClock closes a small loop: a fake that injects a
// clock has to read it back through the same interface the production code uses,
// or a test can be green against a frozen clock the code never consults.
func TestMemoryStateReportsItsOwnClock(t *testing.T) {
	want := time.Date(2026, 5, 1, 8, 30, 0, 0, time.UTC)
	st := NewMemoryStateAt(func() time.Time { return want })

	if got := st.Now(); !got.Equal(want) {
		t.Errorf("Now() = %v, want %v", got, want)
	}

	// And the zero argument gets a working clock rather than a nil-panic on the
	// first marker read.
	if got := NewMemoryStateAt(nil).Now(); got.IsZero() {
		t.Error("NewMemoryStateAt(nil).Now() returned the zero time")
	}
}

// TestMemoryStateEnqueueAtIsAPriorityAndEnqueueIsAnIncrement separates the two
// frontier-writing calls, which look interchangeable and are not.
//
// Enqueue adds to a score, because a URL found by many pages is worth more. EnqueueAt
// sets one, because a retry coming back from the delayed set has a known score and
// re-adding to it would let a retried URL climb without bound.
func TestMemoryStateEnqueueAtIsAPriorityAndEnqueueIsAnIncrement(t *testing.T) {
	st := NewMemoryState()
	ctx := context.Background()

	if err := st.EnqueueAt(ctx, "https://example.com/fixed", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, "https://example.com/fixed"); err != nil {
		t.Fatal(err)
	}
	// One enqueue after EnqueueAt adds to 7 rather than starting from 0.
	if got := st.Frontier()["https://example.com/fixed"]; got != 8 {
		t.Errorf("priority = %v, want 8", got)
	}

	// A zero priority is legal: a URL with no inlinks still has to be crawled
	// eventually, and the frontier is sorted on score so zero puts it last.
	if err := st.EnqueueAt(ctx, "https://example.com/last", 0); err != nil {
		t.Fatal(err)
	}
	if got, present := st.Frontier()["https://example.com/last"]; !present || got != 0 {
		t.Errorf("frontier = %v, want /last present at 0", st.Frontier())
	}
}

// TestMemoryStateHostsListsWhatItKnows covers the inventory call, which is how an
// operator answers "what does this crawler know about".
//
// It is a whole-store scan, so it is only used for reporting -- and that is worth
// asserting, because a frontier entry per host would be enough to break it.
func TestMemoryStateHostsListsWhatItKnows(t *testing.T) {
	st := NewMemoryState()
	ctx := context.Background()

	for _, host := range []string{"a.example", "b.example", "c.example"} {
		if err := st.SaveHostState(ctx, host, HostState{Name: host, MaxPages: 10}); err != nil {
			t.Fatal(err)
		}
	}

	got := st.Hosts()
	want := []string{"a.example", "b.example", "c.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Hosts = %v, want %v", got, want)
	}
	// And the inventory agrees with the records it is inventorying.
	for _, host := range got {
		state, err := st.HostState(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		if state.Name != host {
			t.Errorf("Hosts listed %q but its record says %q", host, state.Name)
		}
	}
}

// TestManagerStateIsTheSameStore is a plumbing assertion with a real purpose.
//
// The crawl loop and the policy manager have to share one store. Two states over
// one Redis is not a subtle bug -- it is two half-populated views of the same
// keys, and every test of the loop would pass while production read counters
// nothing had written.
func TestManagerStateIsTheSameStore(t *testing.T) {
	st := NewMemoryState()
	m := New(DefaultConfig(), st, testLogger())

	if m.State() != State(st) {
		t.Error("State() returned a different store than the one the manager was built with")
	}
}

// TestReasonStringsAreDistinct guards the counters, which are only useful if a
// reason cannot be mistaken for another.
//
// Every Reason is a hash field name, and the stats hash is what an operator reads
// to ask why a site is not being crawled. Two reasons sharing a string would fold
// together with no way to tell which was counted.
func TestReasonStringsAreDistinct(t *testing.T) {
	// Written out rather than derived, because a derived enumeration would only
	// prove the source is self-consistent. This list is a second, independent
	// count of the reasons: adding one to policy.go without adding one here fails
	// the count, and removing one fails too.
	reasons := []Reason{
		ReasonOK,
		ReasonVisited,
		ReasonMalformedURL,
		ReasonNonHTTPScheme,
		ReasonHostDead,
		ReasonHostCold,
		ReasonHostCoolingDown,
		ReasonHostBudgetExhausted,
		ReasonHostMetadataUnknown,
		ReasonRobotsDisallow,
		ReasonPathDisallowed,
		ReasonExtensionSkipped,
		ReasonLanguageNotEnglish,
		ReasonNotInScope,
		ReasonQueryParamFiltered,
		ReasonPolicyUnavailable,
		ReasonRobotsUnreachableReason,

		ReasonFetchOK,
		ReasonNotFound,
		ReasonGone,
		ReasonForbidden,
		ReasonUnauthorized,
		ReasonBadRequest,
		ReasonServerError,
		ReasonRateLimited,
		ReasonDNSFailure,
		ReasonConnectionRefused,
		ReasonTLSError,
		ReasonTimeout,
		ReasonRedirectLoop,
		ReasonBodyTooLarge,
		ReasonContentTypeRejected,
		ReasonAttemptsExhausted,
		ReasonBodyUnparseable,
	}

	seen := map[Reason]bool{}
	for _, r := range reasons {
		if r == "" {
			t.Error("a reason has an empty string; it would count under a key " +
				"no operator would ever query")
			continue
		}
		if seen[r] {
			t.Errorf("reason %q is listed twice", r)
		}
		seen[r] = true
	}
	if len(reasons) != seenCount(t) {
		t.Errorf("this test lists %d reasons, policy.go declares %d", len(reasons), seenCount(t))
	}
}

// seenCount counts the distinct Reason constants in policy.go by reading the
// source, so the comparison above catches a reason added without being listed.
func seenCount(t *testing.T) int {
	t.Helper()

	src, err := os.ReadFile("policy.go")
	if err != nil {
		t.Fatalf("read policy.go: %v", err)
	}
	return strings.Count(string(src), "Reason = \"")
}

// TestVerdictReasonIsAlwaysSetOnARefusal guards the field the whole counting scheme
// rests on.
//
// A refusal with an empty reason counts under "", which is a key no one queries,
// so the refusal happens invisibly -- the failure mode this package exists to
// remove, reintroduced through a missing assignment.
func TestVerdictReasonIsAlwaysSetOnARefusal(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())

	urls := []string{
		"https://example.com/login",
		"https://example.com/a.pdf",
		"mailto:a@example.com",
		"#frag",
		"https://example.com/robots-blocked",
	}
	for _, u := range urls {
		v := mustAdmit(t, m, u)
		if v.Kind == Allow {
			continue
		}
		if v.Reason == "" {
			t.Errorf("%s was refused as %s with no reason", u, v.Kind)
		}
		if !strings.HasPrefix(string(v.Reason), "host_") && v.Reason == ReasonOK {
			t.Errorf("%s was refused with the success reason", u)
		}
	}
}
