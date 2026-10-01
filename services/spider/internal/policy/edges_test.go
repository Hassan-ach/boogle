package policy

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

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

func TestRobotsURLForKeepsWhatItShould(t *testing.T) {
	cases := []struct{ host, want string }{
		{"example.com", "https://example.com/robots.txt"},
		{"example.com:8080", "https://example.com:8080/robots.txt"},
		{"example.com:443", "https://example.com:443/robots.txt"},
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

func TestParseRobotsSkipsANamelessGroup(t *testing.T) {
	got := parseRobots("User-agent:\nDisallow: /\n\nUser-agent: *\nDisallow: /x\n", "BoogleBot")
	if len(got.Disallow) == 0 {
		t.Fatal("nothing was parsed")
	}
	if equalStrings(got.Disallow, []string{"/"}) {
		t.Error("a nameless group was applied; a blank agent is not a wildcard")
	}
}

func TestParseRobotsOnlyAppliesEachGroupOnce(t *testing.T) {
	got := parseRobots(
		"User-agent: BoogleBot\nDisallow: /a\n\nUser-agent: BoogleBot\nDisallow: /b\n",
		"BoogleBot")
	if want := []string{"/a", "/b"}; !equalStrings(got.Disallow, want) {
		t.Errorf("Disallow = %#v, want %#v", got.Disallow, want)
	}
}

func TestDropsQueryParam(t *testing.T) {
	r := DefaultRules()

	for _, q := range []string{"sort", "page", "filter", "q", "search"} {
		if !r.DropsQueryParam(q) {
			t.Errorf("DropsQueryParam(%q) = false, want true", q)
		}
	}

	for _, q := range []string{
		"", "id", "lang", "v",
		"page_size", "sort_order", "query", "searching",
		"utm_source", "fbclid",
	} {
		if r.DropsQueryParam(q) {
			t.Errorf("DropsQueryParam(%q) = true, want false", q)
		}
	}
}

func TestDropsQueryParamOnAnEmptyTable(t *testing.T) {
	var empty RuleSet
	for _, q := range []string{"", "sort", "anything"} {
		if empty.DropsQueryParam(q) {
			t.Errorf("an empty table dropped %q", q)
		}
	}
}

func TestRobotsStaleTreatsANeverReadHostAsStale(t *testing.T) {
	m, _ := newTestManager(t)

	if !m.robotsStale(HostState{Name: "h.example"}) {
		t.Error("a host that was never read was reported as fresh; its rules " +
			"would be treated as an empty set that permits everything")
	}

	if m.robotsStale(HostState{Name: "h.example", RobotsFetchedAt: m.now()}) {
		t.Error("a robots.txt read a moment ago was reported as stale")
	}
}

func TestMemoryStateReportsItsOwnClock(t *testing.T) {
	want := time.Date(2026, 5, 1, 8, 30, 0, 0, time.UTC)
	st := NewMemoryStateAt(func() time.Time { return want })

	if got := st.Now(); !got.Equal(want) {
		t.Errorf("Now() = %v, want %v", got, want)
	}

	if got := NewMemoryStateAt(nil).Now(); got.IsZero() {
		t.Error("NewMemoryStateAt(nil).Now() returned the zero time")
	}
}

func TestMemoryStateEnqueueAtIsAPriorityAndEnqueueIsAnIncrement(t *testing.T) {
	st := NewMemoryState()
	ctx := context.Background()

	if err := st.EnqueueAt(ctx, "https://example.com/fixed", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, "https://example.com/fixed"); err != nil {
		t.Fatal(err)
	}
	if got := st.Frontier()["https://example.com/fixed"]; got != 8 {
		t.Errorf("priority = %v, want 8", got)
	}

	if err := st.EnqueueAt(ctx, "https://example.com/last", 0); err != nil {
		t.Fatal(err)
	}
	if got, present := st.Frontier()["https://example.com/last"]; !present || got != 0 {
		t.Errorf("frontier = %v, want /last present at 0", st.Frontier())
	}
}

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

func TestManagerStateIsTheSameStore(t *testing.T) {
	st := NewMemoryState()
	m := New(DefaultConfig(), st, testLogger())

	if m.State() != State(st) {
		t.Error("State() returned a different store than the one the manager was built with")
	}
}

func TestReasonStringsAreDistinct(t *testing.T) {
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

func seenCount(t *testing.T) int {
	t.Helper()

	src, err := os.ReadFile("policy.go")
	if err != nil {
		t.Fatalf("read policy.go: %v", err)
	}
	return strings.Count(string(src), "Reason = \"")
}

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
