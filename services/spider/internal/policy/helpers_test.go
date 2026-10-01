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

type robotsResponse struct {
	body   string
	status int
	err    error
}

type fakeRobots struct {
	mu     sync.Mutex
	reply  map[string]robotsResponse
	body   string
	status int

	calls    []string
	failCall bool
}

func newFakeRobots(body string) *fakeRobots {
	return &fakeRobots{reply: map[string]robotsResponse{}, body: body, status: 200}
}

func (f *fakeRobots) script(host string, r robotsResponse) *fakeRobots {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply[host] = r
	return f
}

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
			return nil, 0, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return []byte(f.body), f.status, nil
	}
}

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

func (f *fakeRobots) forbidCalls(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCall = true
}

func hostOfRobotsURL(raw string) string {
	rest := strings.TrimPrefix(raw, "https://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

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

func assertVerdict(t *testing.T, got *Verdict, wantKind VerdictKind, wantReason Reason) {
	t.Helper()
	if got.Kind != wantKind {
		t.Errorf("kind = %v (%s), want %v", got.Kind, got.Kind, wantKind)
	}
	if got.Reason != wantReason {
		t.Errorf("reason = %q, want %q", got.Reason, wantReason)
	}
}

type clock struct {
	mu sync.Mutex
	t  int64
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

func atClock(t *testing.T, now time.Time) (*PolicyManager, *MemoryState, *clock) {
	t.Helper()
	c := newClock(now)
	st := NewMemoryStateAt(c.Now)
	m := New(DefaultConfig(), st, testLogger()).
		WithClock(c.Now).
		WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}}).fetcher())
	return m, st, c
}

func admitReason(t *testing.T, m *PolicyManager, rawURL string) (VerdictKind, Reason) {
	t.Helper()
	v := mustAdmit(t, m, rawURL)
	return v.Kind, v.Reason
}

func itoa(n int) string { return strconv.Itoa(n) }

var uniquePath atomic.Int64

func uniqueURL(host, path string) string {
	return "https://" + host + path + "?n=" + strconv.FormatInt(uniquePath.Add(1), 10)
}
