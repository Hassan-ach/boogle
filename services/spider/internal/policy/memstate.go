package policy

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryState is the in-process State implementation, used by tests and by
// single-node runs. mu is not a RWMutex because expired() mutates the marker
// maps it inspects, so even reads need the write lock.
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

	// FailWith and FailOn are fault-injection hooks: FailWith fails every
	// operation, FailOn fails the named ones. beginOp consults them so a test
	// can exercise error paths without a broken Redis. Production leaves both
	// nil/empty.
	FailWith error

	FailOn map[string]error

	closed bool
}

func NewMemoryState() *MemoryState {
	return NewMemoryStateAt(time.Now)
}

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

func (m *MemoryState) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.now().Add(d)
	m.now = func() time.Time { return next }
}

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

// beginOp is the single gate every operation passes through. It must be called
// while holding mu, because it reads FailWith, FailOn and closed. begin() is
// the same gate for operations that need no named fault.
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

// expired reports whether a marker has lapsed, deleting it as a side effect so
// the map cannot grow without bound. Callers hold mu; a true return means the
// marker is gone, not merely due for deletion.
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
	if err := m.beginOp("MarkVisited"); err != nil {
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
	if err := m.beginOp("EnqueueDelayed"); err != nil {
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
	if err := m.beginOp("FrontierLen"); err != nil {
		return 0, err
	}
	return int64(len(m.frontier)), nil
}

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
	if err := m.beginOp("URLState"); err != nil {
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
	if err := m.beginOp("BumpAttempts"); err != nil {
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
	if err := m.beginOp("ClearURLState"); err != nil {
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
	if err := m.beginOp("SaveHostState"); err != nil {
		return err
	}
	if host == "" {
		return errEmptyHost
	}
	st.Name = host
	st.SiteMapsClaimedAt = m.host[host].SiteMapsClaimedAt
	m.host[host] = st
	return nil
}

func (m *MemoryState) RecordSuccess(ctx context.Context, host string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("RecordSuccess"); err != nil {
		return err
	}
	if host == "" {
		return nil
	}
	st := m.host[host]
	st.Name = host
	st.PagesCrawled++
	st.ConsecFailures = 0
	st.LastSuccess = at
	st.Status = statusReady
	m.host[host] = st
	return nil
}

func (m *MemoryState) RecordFailure(ctx context.Context, host string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("RecordFailure"); err != nil {
		return 0, err
	}
	if host == "" {
		return 0, nil
	}
	st := m.host[host]
	st.Name = host
	st.ConsecFailures++
	st.Status = statusDegraded
	m.host[host] = st
	return st.ConsecFailures, nil
}

func (m *MemoryState) ClaimSiteMaps(ctx context.Context, host string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.beginOp("ClaimSiteMaps"); err != nil {
		return false, err
	}
	if host == "" || at.IsZero() {
		return false, nil
	}
	st := m.host[host]
	if !st.SiteMapsClaimedAt.Before(at) {
		return false, nil
	}
	st.Name = host
	st.SiteMapsClaimedAt = at
	m.host[host] = st
	return true, nil
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
	if err := m.beginOp("ClearMarker"); err != nil {
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

func (m *MemoryState) Stats(host string) map[Reason]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Reason]int, len(m.stats[host]))
	for k, v := range m.stats[host] {
		out[k] = v
	}
	return out
}

func (m *MemoryState) StatsTotal(host string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, v := range m.stats[host] {
		total += v
	}
	return total
}

func (m *MemoryState) Frontier() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.frontier))
	for k, v := range m.frontier {
		out[k] = v
	}
	return out
}

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

func (m *MemoryState) Delayed() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time, len(m.delayed))
	for k, v := range m.delayed {
		out[k] = v
	}
	return out
}

func (m *MemoryState) Visited() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.visited))
	for k := range m.visited {
		out[k] = struct{}{}
	}
	return out
}

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
