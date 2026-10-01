package policy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// robotsResponse is one scripted answer from fakeRobots.
type robotsResponse struct {
	body   string
	status int
	err    error
}

// fakeRobots is a scripted robots.txt server.
//
// It exists because EnsureHost's behaviour is entirely about *when* a fetch
// happens, and that cannot be tested against a real host: the interesting cases
// are "the second URL does not fetch", "a host that failed does not fetch again
// for an hour" and "a host that was never resolved fetches exactly once". Those
// are assertions about a call count, so the fake counts calls.
type fakeRobots struct {
	mu    sync.Mutex
	reply map[string]robotsResponse
	// body is the fallback for a host with no scripted entry. An empty body with
	// status 0 means "not found", so an unscripted host is a dead host -- the case
	// most worth catching by accident.
	body   string
	status int

	calls    []string
	failCall bool
}

func newFakeRobots(body string) *fakeRobots {
	return &fakeRobots{reply: map[string]robotsResponse{}, body: body, status: 200}
}

// script pins a response for one host.
func (f *fakeRobots) script(host string, r robotsResponse) *fakeRobots {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply[host] = r
	return f
}

// fetcher turns the script into a RobotsFetcher.
func (f *fakeRobots) fetcher() RobotsFetcher {
	return func(_ context.Context, rawURL string) ([]byte, int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()

		host := hostOfRobotsURL(rawURL)
		f.calls = append(f.calls, host)

		if f.failCall {
			return nil, 0, errors.New("the test asked every robots fetch to fail")
		}
		if r, ok := f.reply[host]; ok {
			if r.err != nil {
				return nil, r.status, r.err
			}
			status := r.status
			if status == 0 {
				status = 200
			}
			return []byte(r.body), status, nil
		}
		if f.status == 0 {
			// A real *net.DNSError, not a lookalike, because the point of an
			// unscripted host here is to exercise the classification path a dead
			// domain actually takes -- a stand-in error would be classified as
			// "unrecognised" and take the generic backoff branch instead.
			return nil, 0, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return []byte(f.body), f.status, nil
	}
}

// callCount is how many times a host's robots.txt was fetched.
func (f *fakeRobots) callCount(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == host {
			n++
		}
	}
	return n
}

func (f *fakeRobots) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// forbidCalls makes every subsequent fetch report that the test is wrong if it
// happens at all. It is how the ordering assertion is written: not "the fetch
// returned an error we handled", but "the fetch did not occur".
func (f *fakeRobots) forbidCalls(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCall = true
}

// hostOfRobotsURL pulls the host back out of a robots.txt URL.
func hostOfRobotsURL(raw string) string {
	rest := strings.TrimPrefix(raw, "https://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// mustAdmit runs Admit and fails the test on an error, returning the verdict.
func mustAdmit(t *testing.T, m *PolicyManager, rawURL string) *Verdict {
	t.Helper()
	v, err := m.Admit(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("Admit(%q) returned an error: %v", rawURL, err)
	}
	if v == nil {
		t.Fatalf("Admit(%q) returned a nil verdict", rawURL)
	}
	return v
}

// assertVerdict checks a verdict's kind and reason together.
//
// Both are checked because a verdict with the right kind and the wrong reason is
// still a bug: the reason is what the stats hash counts, so a mislabelled refusal
// is a mislabelled number in the one place an operator would look.
func assertVerdict(t *testing.T, got *Verdict, wantKind VerdictKind, wantReason Reason) {
	t.Helper()
	if got.Kind != wantKind {
		t.Errorf("kind = %v (%s), want %v", got.Kind, got.Kind, wantKind)
	}
	if got.Reason != wantReason {
		t.Errorf("reason = %q, want %q", got.Reason, wantReason)
	}
}

// clock is a manually advanced clock shared by a manager and its state, so the
// two never disagree about what time it is.
type clock struct {
	mu sync.Mutex
	t  int64 // unix nanos
}

func newClock(t time.Time) *clock { return &clock{t: t.UnixNano()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(0, c.t).UTC()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t += d.Nanoseconds()
}

// atClock builds a manager and a state reading one clock.
func atClock(t *testing.T, now time.Time) (*PolicyManager, *MemoryState, *clock) {
	t.Helper()
	c := newClock(now)
	st := NewMemoryStateAt(c.Now)
	m := New(DefaultConfig(), st, testLogger()).
		WithClock(c.Now).
		WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}}).fetcher())
	return m, st, c
}

// admitReason runs Admit and returns the kind and reason, for tests that only
// care about the classification.
func admitReason(t *testing.T, m *PolicyManager, rawURL string) (VerdictKind, Reason) {
	t.Helper()
	v := mustAdmit(t, m, rawURL)
	return v.Kind, v.Reason
}

// itoa is strconv.Itoa for the small positive counters tests build paths from.
func itoa(n int) string { return strconv.Itoa(n) }

// uniquePath builds a URL that has not been admitted before, so a test can admit
// the same page twice without the second call tripping the visited check.
//
// The counter is per-process, which is all a test needs: within one run no other
// test is handing out the same path, and across runs the state is fresh too.
var uniquePath atomic.Int64

func uniqueURL(host, path string) string {
	return "https://" + host + path + "?n=" + strconv.FormatInt(uniquePath.Add(1), 10)
}
