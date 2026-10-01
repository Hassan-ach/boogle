package policy

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryState is an in-memory State for tests.
//
// It exists because the policy rules are the interesting part of this package
// and they cannot be tested against a real Redis without one running. It is
// deliberately hand-written rather than pulled from a module: the repository
// carries no test dependencies, and adding one to fake a map with a TTL is a poor
// trade.
//
// Nothing in production constructs this. The spider wires RedisState; a
// MemoryState reaching a binary is a wiring mistake.
//
// It is a faithful fake, not a convenient one. TTLs are honoured against an
// injectable clock rather than assumed away, SetMarker does not extend an
// existing marker, and FrontierLen counts what is really enqueued. A fake that
// lied about any of those would let the exact bugs this package exists to fix
// pass their tests.
type MemoryState struct {
	mu sync.Mutex

	now func() time.Time

	visited  map[string]struct{}
	frontier map[string]float64
	delayed  map[string]time.Time
	urlState map[string]URLState
	host     map[string]HostState
	markers  map[string]map[MarkerKind]time.Time
	stats    map[string]map[Reason]int

	// FailWith, when set, makes every method return it. This is how the
	// fail-closed contract gets tested: a caller that treats an unreachable
	// state as "no state, carry on" has to fail.
	FailWith error

	// FailOn fails a single named operation, keyed by the method name. FailWith
	// cannot express "the marker read works but the host-state read does not",
	// and that distinction is what proves the gate reads markers first.
	FailOn map[string]error

	closed bool
}

// NewMemoryState returns an empty MemoryState reading time from now.
func NewMemoryState() *MemoryState {
	return NewMemoryStateAt(time.Now)
}

// NewMemoryStateAt returns a MemoryState whose notion of time is whatever now
// returns. Tests that exercise a TTL, a cooldown or a cold window drive this
// clock directly instead of sleeping.
func NewMemoryStateAt(now func() time.Time) *MemoryState {
	if now == nil {
		now = time.Now
	}
	return &MemoryState{
		now:      now,
		visited:  map[string]struct{}{},
		frontier: map[string]float64{},
		delayed:  map[string]time.Time{},
		urlState: map[string]URLState{},
		host:     map[string]HostState{},
		markers:  map[string]map[MarkerKind]time.Time{},
		stats:    map[string]map[Reason]int{},
		FailOn:   map[string]error{},
	}
}

// Advance moves the clock forward. Expiry is evaluated lazily on read, so
// nothing needs to sweep.
func (m *MemoryState) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The new clock reads the previous one once, then returns a fixed instant.
	// Capturing m.now directly would deadlock: the closure would take the lock
	// that is already held, and sync.Mutex is not reentrant.
	next := m.now().Add(d)
	m.now = func() time.Time { return next }
}

// SetClock replaces the clock outright.
func (m *MemoryState) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func (m *MemoryState) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now()
}

func (m *MemoryState) begin() error {
	return m.beginOp("")
}

// beginOp is begin with the operation name, for FailOn.
func (m *MemoryState) beginOp(op string) error {
	if m.FailWith != nil {
		return m.FailWith
	}
	if err, ok := m.FailOn[op]; ok && err != nil {
		return err
	}
	if m.closed {
		return errMemoryClosed
	}
	return nil
}

// expired reports whether a marker's deadline has passed, deleting it if so.
func (m *MemoryState) expired(host string, kind MarkerKind) bool {
	set, ok := m.markers[host]
	if !ok {
		return true
	}
	until, ok := set[kind]
	if !ok {
		return true
	}
	if !m.now().Before(until) {
		delete(set, kind)
		if len(set) == 0 {
			delete(m.markers, host)
		}
		return true
	}
	return false
}

func (m *MemoryState) MarkVisited(ctx context.Context, urls ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	for _, u := range urls {
		if u != "" {
			m.visited[u] = struct{}{}
		}
	}
	return nil
}

func (m *MemoryState) IsVisited(ctx context.Context, url string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("IsVisited"); err != nil {
		return false, err
	}
	_, ok := m.visited[url]
	return ok, nil
}

func (m *MemoryState) Enqueue(ctx context.Context, urls ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	for _, u := range urls {
		if u != "" {
			m.frontier[u]++
		}
	}
	return nil
}

func (m *MemoryState) EnqueueAt(ctx context.Context, url string, priority float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	if url != "" {
		m.frontier[url] = priority
	}
	return nil
}

func (m *MemoryState) EnqueueDelayed(ctx context.Context, url string, due time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	if url != "" {
		m.delayed[url] = due
	}
	return nil
}

func (m *MemoryState) FrontierLen(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Named, so a test can fail this read alone. That distinction matters for
	// the store's seeding decision, which has to tell "the frontier is empty"
	// from "I could not read the frontier" -- and a fake that can only fail
	// everything at once cannot express a caller that has to make that choice.
	if err := m.beginOp("FrontierLen"); err != nil {
		return 0, err
	}
	return int64(len(m.frontier)), nil
}

// PopFrontier mirrors the Lua script's semantics exactly, including the
// found/exhausted distinction.
//
// The sort is by score descending with member order broken alphabetically, which
// is how Redis orders a ZSET on equal scores. Matching that matters because the
// script and this fake are expected to hand a test the same URL, and a fake that
// ordered differently would hide a scoring bug rather than reproduce it.
func (m *MemoryState) PopFrontier(ctx context.Context, budget int) (PopResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("PopFrontier"); err != nil {
		return PopResult{}, err
	}
	if budget < 1 {
		budget = defaultPopBatch
	}

	order := m.frontierOrderLocked()
	skipped := 0

	for i := 0; i < budget; i++ {
		if len(order) == 0 {
			return PopResult{Exhausted: true, VisitedSkipped: skipped}, nil
		}
		url := order[0]
		order = order[1:]
		delete(m.frontier, url)

		if _, visited := m.visited[url]; visited {
			skipped++
			continue
		}
		return PopResult{
			URL:            url,
			Found:          true,
			VisitedSkipped: skipped,
		}, nil
	}

	// Budget spent on visited entries only. Anything left in the frontier is
	// still there, so this is deliberately not exhausted.
	return PopResult{VisitedSkipped: skipped}, nil
}

func (m *MemoryState) PromoteDelayed(ctx context.Context, now time.Time, batch int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("PromoteDelayed"); err != nil {
		return 0, err
	}
	if batch < 1 {
		batch = defaultPromoteBatch
	}

	// Only entries actually due, and only as many as the batch allows. Sorting
	// first makes the truncation deterministic; the Lua version's ZRANGEBYSCORE
	// LIMIT breaks ties by member, which this matches by taking the lowest member
	// among equal due times.
	due := make([]string, 0, len(m.delayed))
	for url, at := range m.delayed {
		if !at.After(now) {
			due = append(due, url)
		}
	}
	sort.Strings(due)
	if len(due) > batch {
		due = due[:batch]
	}

	for _, url := range due {
		// ZADD NX: a URL discovered again while parked keeps the score it
		// earned from its inlinks rather than being reset to zero.
		if _, exists := m.frontier[url]; !exists {
			m.frontier[url] = 0
		}
		delete(m.delayed, url)
	}
	return len(due), nil
}

func (m *MemoryState) URLState(ctx context.Context, url string) (URLState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return URLState{}, err
	}
	if st, ok := m.urlState[url]; ok {
		return st, nil
	}
	return URLState{URL: url}, nil
}

func (m *MemoryState) BumpAttempts(ctx context.Context, url string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return 0, err
	}
	if url == "" {
		return 0, nil
	}
	st := m.urlState[url]
	st.URL = url
	st.Attempts++
	st.EnqueuedAt = m.now()
	m.urlState[url] = st
	return st.Attempts, nil
}

func (m *MemoryState) ClearURLState(ctx context.Context, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	delete(m.urlState, url)
	return nil
}

func (m *MemoryState) HostState(ctx context.Context, host string) (HostState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("HostState"); err != nil {
		return HostState{}, err
	}
	if st, ok := m.host[host]; ok {
		return st, nil
	}
	return HostState{Name: host}, nil
}

func (m *MemoryState) SaveHostState(ctx context.Context, host string, st HostState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	if host == "" {
		return errEmptyHost
	}
	st.Name = host
	m.host[host] = st
	return nil
}

func (m *MemoryState) IncrFailures(ctx context.Context, host string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return 0, err
	}
	if host == "" {
		return 0, nil
	}
	st := m.host[host]
	st.Name = host
	st.ConsecFailures++
	m.host[host] = st
	return st.ConsecFailures, nil
}

func (m *MemoryState) ResetFailures(ctx context.Context, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	st := m.host[host]
	st.ConsecFailures = 0
	if st.Name == "" {
		st.Name = host
	}
	m.host[host] = st
	return nil
}

func (m *MemoryState) IncrPagesCrawled(ctx context.Context, host string, n int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return 0, err
	}
	if host == "" || n == 0 {
		return 0, nil
	}
	st := m.host[host]
	st.Name = host
	st.PagesCrawled += int(n)
	m.host[host] = st
	return st.PagesCrawled, nil
}

func (m *MemoryState) ResetWindow(ctx context.Context, host string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("ResetWindow"); err != nil {
		return err
	}
	st := m.host[host]
	if st.Name == "" {
		st.Name = host
	}
	st.PagesCrawled = 0
	st.WindowStartedAt = at
	m.host[host] = st
	return nil
}

func (m *MemoryState) Markers(ctx context.Context, host string) (map[MarkerKind]time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("Markers"); err != nil {
		return nil, err
	}
	out := map[MarkerKind]time.Duration{}
	if host == "" {
		return out, nil
	}
	for _, kind := range AllMarkers {
		if m.expired(host, kind) {
			continue
		}
		out[kind] = m.markers[host][kind].Sub(m.now())
	}
	return out, nil
}

func (m *MemoryState) SetMarker(ctx context.Context, host string, kind MarkerKind, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	if _, ok := kind.suffix(); !ok {
		return errUnknownMarker
	}
	if host == "" {
		return nil
	}
	if ttl <= 0 {
		return m.clearMarkerLocked(host, kind)
	}
	// NX semantics: an existing marker keeps its deadline. Twenty workers
	// marking one host dead must not push the deadline out to whoever ran last.
	if m.expired(host, kind) {
		if m.markers[host] == nil {
			m.markers[host] = map[MarkerKind]time.Time{}
		}
		m.markers[host][kind] = m.now().Add(ttl)
	}
	return nil
}

func (m *MemoryState) ClearMarker(ctx context.Context, host string, kind MarkerKind) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin(); err != nil {
		return err
	}
	if _, ok := kind.suffix(); !ok {
		return errUnknownMarker
	}
	return m.clearMarkerLocked(host, kind)
}

func (m *MemoryState) clearMarkerLocked(host string, kind MarkerKind) error {
	if set, ok := m.markers[host]; ok {
		delete(set, kind)
		if len(set) == 0 {
			delete(m.markers, host)
		}
	}
	return nil
}

func (m *MemoryState) CountReason(ctx context.Context, host string, reason Reason) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("CountReason"); err != nil {
		return err
	}
	if host == "" || reason == "" {
		return nil
	}
	if m.stats[host] == nil {
		m.stats[host] = map[Reason]int{}
	}
	m.stats[host][reason]++
	return nil
}

func (m *MemoryState) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// --- inspection helpers, for tests only ---

// Stats returns a copy of the per-host reason counters.
func (m *MemoryState) Stats(host string) map[Reason]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Reason]int, len(m.stats[host]))
	for k, v := range m.stats[host] {
		out[k] = v
	}
	return out
}

// StatsTotal sums every reason counter for a host.
func (m *MemoryState) StatsTotal(host string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, v := range m.stats[host] {
		total += v
	}
	return total
}

// Frontier returns the enqueued URLs with their priorities.
func (m *MemoryState) Frontier() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.frontier))
	for k, v := range m.frontier {
		out[k] = v
	}
	return out
}

// FrontierOrder returns the frontier's URLs from highest to lowest priority,
// with member order broken alphabetically so the result is deterministic.
func (m *MemoryState) FrontierOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frontierOrderLocked()
}

func (m *MemoryState) frontierOrderLocked() []string {
	out := make([]string, 0, len(m.frontier))
	for u := range m.frontier {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if m.frontier[out[i]] != m.frontier[out[j]] {
			return m.frontier[out[i]] > m.frontier[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// Delayed returns the parked URLs and their due times.
func (m *MemoryState) Delayed() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time, len(m.delayed))
	for k, v := range m.delayed {
		out[k] = v
	}
	return out
}

// Visited returns the terminal URL set.
func (m *MemoryState) Visited() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.visited))
	for k := range m.visited {
		out[k] = struct{}{}
	}
	return out
}

// Hosts returns the hosts with a stored record.
func (m *MemoryState) Hosts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.host))
	for h := range m.host {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
