package policy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisState is the Redis-backed State.
//
// It holds no policy. Every method here is a fact about stored state; the
// decisions that consume these facts are Admit and Classify. Keeping that line
// sharp is what lets the rules be tested with a stub and the storage be tested
// against a real Redis, neither dragging the other along.
type RedisState struct {
	conn *redis.Client
	keys Keyspace
	// urlTTL bounds how long per-URL retry bookkeeping survives. Without it a
	// long crawl accumulates one hash per URL it ever touched, for URLs it
	// stopped caring about weeks ago.
	urlTTL time.Duration
}

// NewRedisState wraps an existing client. The caller keeps ownership of the
// connection: closing it is the spider's business, not the policy's.
func NewRedisState(conn *redis.Client, prefix string, urlTTL time.Duration) *RedisState {
	if urlTTL <= 0 {
		urlTTL = 7 * 24 * time.Hour
	}
	return &RedisState{
		conn:   conn,
		keys:   NewKeyspace(prefix),
		urlTTL: urlTTL,
	}
}

// Keys exposes the keyspace so callers can read and assert on the exact keys
// this implementation uses. Tests need it; production does not.
func (s *RedisState) Keys() Keyspace { return s.keys }

func (s *RedisState) Close() error { return s.conn.Close() }

// --- terminal URL record ---

func (s *RedisState) MarkVisited(ctx context.Context, urls ...string) error {
	if len(urls) == 0 {
		return nil
	}
	// Empty strings are dropped rather than added: a visited set is a
	// correctness structure, and an empty member would match a URL that failed
	// to parse, which is exactly the case the visited set is meant to exclude.
	members := make([]any, 0, len(urls))
	for _, u := range urls {
		if u != "" {
			members = append(members, u)
		}
	}
	if len(members) == 0 {
		return nil
	}
	if err := s.conn.SAdd(ctx, s.keys.Visited(), members...).Err(); err != nil {
		return fmt.Errorf("mark visited: %w", err)
	}
	return nil
}

func (s *RedisState) IsVisited(ctx context.Context, url string) (bool, error) {
	if url == "" {
		return false, nil
	}
	ok, err := s.conn.SIsMember(ctx, s.keys.Visited(), url).Result()
	if err != nil {
		return false, fmt.Errorf("check visited: %w", err)
	}
	return ok, nil
}

// --- frontier ---

func (s *RedisState) Enqueue(ctx context.Context, urls ...string) error {
	if len(urls) == 0 {
		return nil
	}
	// One pipeline for the whole batch. A page with four hundred links costs
	// one round trip rather than four hundred, which at twenty workers is the
	// difference between the crawl being Redis-bound and being fine.
	pipe := s.conn.Pipeline()
	for _, u := range urls {
		if u != "" {
			pipe.ZIncrBy(ctx, s.keys.Frontier(), 1, u)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

func (s *RedisState) EnqueueAt(ctx context.Context, url string, priority float64) error {
	if url == "" {
		return nil
	}
	if err := s.conn.ZAdd(ctx, s.keys.Frontier(), redis.Z{
		Score:  priority,
		Member: url,
	}).Err(); err != nil {
		return fmt.Errorf("enqueue at %.2f: %w", priority, err)
	}
	return nil
}

func (s *RedisState) EnqueueDelayed(ctx context.Context, url string, due time.Time) error {
	if url == "" {
		return nil
	}
	// Score is the due time in unix seconds, which is what lets one ZRANGEBYSCORE
	// find everything that has come due, with no timer and no per-URL bookkeeping.
	if err := s.conn.ZAdd(ctx, s.keys.Delayed(), redis.Z{
		Score:  float64(due.Unix()),
		Member: url,
	}).Err(); err != nil {
		return fmt.Errorf("enqueue delayed until %s: %w", due.UTC().Format(time.RFC3339), err)
	}
	return nil
}

func (s *RedisState) FrontierLen(ctx context.Context) (int64, error) {
	n, err := s.conn.ZCard(ctx, s.keys.Frontier()).Result()
	if err != nil {
		return 0, fmt.Errorf("frontier length: %w", err)
	}
	return n, nil
}

// --- per-URL retry bookkeeping ---

func (s *RedisState) URLState(ctx context.Context, url string) (URLState, error) {
	fields, err := s.conn.HGetAll(ctx, s.keys.URLState(url)).Result()
	if err != nil {
		return URLState{}, fmt.Errorf("read url state: %w", err)
	}
	if len(fields) == 0 {
		// No record is the normal case for a URL we have not retried.
		return URLState{URL: url}, nil
	}

	st := URLState{
		URL:        fields[fieldURL],
		Attempts:   atoiDefault(fields[fieldAttempts], 0),
		LastStatus: atoiDefault(fields[fieldLastStatus], 0),
		LastError:  fields[fieldLastError],
	}
	if v, err := strconv.ParseInt(fields[fieldEnqueuedAt], 10, 64); err == nil && v > 0 {
		st.EnqueuedAt = time.Unix(v, 0).UTC()
	}
	if st.URL == "" {
		st.URL = url
	}
	return st, nil
}

func (s *RedisState) BumpAttempts(ctx context.Context, url string) (int, error) {
	if url == "" {
		return 0, nil
	}
	key := s.keys.URLState(url)

	// HINCRBY and EXPIRE in one pipeline, so a failed increment cannot leave a
	// record without a TTL, and the TTL is refreshed on every failure rather
	// than only on creation.
	pipe := s.conn.Pipeline()
	incr := pipe.HIncrBy(ctx, key, fieldAttempts, 1)
	pipe.Expire(ctx, key, s.urlTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("bump attempts: %w", err)
	}
	// HINCRBY without a pre-existing hash starts the field at 0 and then
	// increments, so this returns 1 on the first failure. The URL is stored too
	// so the key can be traced back to its member.
	if n, err := incr.Result(); err == nil {
		_ = s.conn.HSet(ctx, key, fieldURL, url, fieldEnqueuedAt, time.Now().Unix()).Err()
		return int(n), nil
	}
	return 0, fmt.Errorf("bump attempts: %w", incr.Err())
}

func (s *RedisState) ClearURLState(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	if err := s.conn.Del(ctx, s.keys.URLState(url)).Err(); err != nil {
		return fmt.Errorf("clear url state: %w", err)
	}
	return nil
}

// --- per-host policy state ---

func (s *RedisState) HostState(ctx context.Context, host string) (HostState, error) {
	fields, err := s.conn.HGetAll(ctx, s.keys.HostState(host)).Result()
	if err != nil {
		return HostState{}, fmt.Errorf("read host state: %w", err)
	}
	st := HostState{Name: host}
	if len(fields) == 0 {
		// An unknown host is a normal condition, not an error. Admit resolves it
		// by fetching robots.txt, and the alternative -- an error here -- would
		// make every first visit look like a Redis outage.
		return st, nil
	}

	st.CrawlDelay = time.Duration(atoiDefault(fields[fieldCrawlDelay], 0)) * time.Millisecond
	st.MaxPages = atoiDefault(fields[fieldMaxPages], 0)
	st.PagesCrawled = atoiDefault(fields[fieldPagesCrawled], 0)
	st.ConsecFailures = atoiDefault(fields[fieldFailures], 0)
	st.Status = fields[fieldStatus]
	st.Allow = splitLines(fields[fieldAllow])
	st.Disallow = splitLines(fields[fieldDisallow])
	st.SiteMaps = splitLines(fields[fieldSitemaps])
	st.SiteMapsClaimedAt = parseUnix(fields[fieldSitemapsClaimedAt])
	st.WindowStartedAt = parseUnix(fields[fieldWindowStart])
	st.RobotsFetchedAt = parseUnix(fields[fieldRobotsAt])
	st.FirstSeen = parseUnix(fields[fieldFirstSeen])
	st.LastSuccess = parseUnix(fields[fieldLastSuccess])
	return st, nil
}

// SaveHostState writes the fields a robots.txt resolution owns and nothing else.
//
// The counters are deliberately absent. They are moved by RecordSuccess and
// RecordFailure, which every worker calls concurrently, and this method's
// argument is a record read at some earlier moment -- so writing pages_crawled or
// consec_failures from it is a read-modify-write that loses whichever increment
// arrived second. That is not a rare race: twenty workers on one host is the
// normal case, and the loser is not a page but a count, so the budget silently
// drifts and a host spends longer than MAX_PAGES_PER_HOST before going cold.
//
// SiteMapsClaimedAt is absent for the same reason. It is a claim, won by one
// worker against all the others, so writing it from here -- with a value read
// before the claim was taken -- would erase a claim another worker holds, and the
// erase would land between that worker's claim and its work.
//
// A zero value in the fields written here is meaningful (no crawl delay, no
// host-specific budget), so "leave it alone" is not available as an option the
// way it is for the timestamps.
func (s *RedisState) SaveHostState(ctx context.Context, host string, st HostState) error {
	if host == "" {
		return errors.New("save host state: empty host")
	}

	values := map[string]any{
		fieldMaxPages:   st.MaxPages,
		fieldCrawlDelay: st.CrawlDelay.Milliseconds(),
	}
	// Only the optional fields are written when set. A zero time is not a fact
	// about the host, and writing it would overwrite a real timestamp with the
	// epoch.
	if !st.WindowStartedAt.IsZero() {
		values[fieldWindowStart] = st.WindowStartedAt.Unix()
	}
	if !st.RobotsFetchedAt.IsZero() {
		values[fieldRobotsAt] = st.RobotsFetchedAt.Unix()
	}
	if !st.FirstSeen.IsZero() {
		values[fieldFirstSeen] = st.FirstSeen.Unix()
	}
	if st.Status != "" {
		values[fieldStatus] = st.Status
	}
	if len(st.Allow) > 0 {
		values[fieldAllow] = strings.Join(st.Allow, "\n")
	}
	if len(st.Disallow) > 0 {
		values[fieldDisallow] = strings.Join(st.Disallow, "\n")
	}
	if len(st.SiteMaps) > 0 {
		values[fieldSitemaps] = strings.Join(st.SiteMaps, "\n")
	}

	if err := s.conn.HSet(ctx, s.keys.HostState(host), values).Err(); err != nil {
		return fmt.Errorf("save host state for %s: %w", host, err)
	}
	return nil
}

// RecordSuccess applies a successful page in one round trip.
//
// HINCRBY and HSET are in a single script rather than a pipeline because the
// count and the timestamp have to agree: a page counted without its timestamp
// recorded reads as a host that is crawling but has never worked, and a
// timestamp recorded without the count throws away the page the budget was
// spending. Neither is recoverable by a later read, because the read would find
// a state neither of them left alone.
var recordSuccessScript = redis.NewScript(`
local key = KEYS[1]
redis.call("hincrby", key, ARGV[1], 1)
redis.call("hset", key, ARGV[2], 0, ARGV[3], ARGV[4], ARGV[5], ARGV[6])
return 1
`)

func (s *RedisState) RecordSuccess(ctx context.Context, host string, at time.Time) error {
	if host == "" {
		return nil
	}
	if err := recordSuccessScript.Run(ctx, s.conn,
		[]string{s.keys.HostState(host)},
		fieldPagesCrawled,
		fieldFailures,
		fieldLastSuccess,
		at.Unix(),
		fieldStatus,
		statusReady,
	).Err(); err != nil {
		return fmt.Errorf("record a success for %s: %w", host, err)
	}
	return nil
}

func (s *RedisState) RecordFailure(ctx context.Context, host string) (int, error) {
	if host == "" {
		return 0, nil
	}
	// The count and the status move together for the same reason as RecordSuccess:
	// a host counted as failing but still labelled ready is a state the rest of
	// this package cannot reason about, and the only repair would be a read that
	// races the next write.
	pipe := s.conn.Pipeline()
	incr := pipe.HIncrBy(ctx, s.keys.HostState(host), fieldFailures, 1)
	pipe.HSet(ctx, s.keys.HostState(host), fieldStatus, statusDegraded)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("record a failure for %s: %w", host, err)
	}
	n, err := incr.Result()
	if err != nil {
		return 0, fmt.Errorf("record a failure for %s: %w", host, err)
	}
	return int(n), nil
}

// claimSiteMapsScript compares and sets the claim in one step.
//
// HSETNX would do, if the claim were a flag that is only ever set once. It is not:
// a new robots.txt has to win the claim again, and doing that with a flag needs a
// clear-then-claim pair that is two writes and therefore two races. Twenty workers
// reaching a brand-new host is the ordinary case, not the pathological one, so the
// pair fires.
var claimSiteMapsScript = redis.NewScript(`
local claimed = redis.call("hget", KEYS[1], ARGV[1])
if not claimed or claimed == false or claimed == "" then
  redis.call("hset", KEYS[1], ARGV[1], ARGV[2])
  return 1
end
if tonumber(claimed) < tonumber(ARGV[2]) then
  redis.call("hset", KEYS[1], ARGV[1], ARGV[2])
  return 1
end
return 0
`)

func (s *RedisState) ClaimSiteMaps(ctx context.Context, host string, at time.Time) (bool, error) {
	if host == "" || at.IsZero() {
		return false, nil
	}
	won, err := claimSiteMapsScript.Run(ctx, s.conn,
		[]string{s.keys.HostState(host)},
		fieldSitemapsClaimedAt, at.Unix(),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("claim sitemaps for %s: %w", host, err)
	}
	return won == 1, nil
}

func (s *RedisState) ResetWindow(ctx context.Context, host string, at time.Time) error {
	if host == "" {
		return nil
	}
	err := s.conn.HSet(ctx, s.keys.HostState(host),
		fieldPagesCrawled, 0,
		fieldWindowStart, at.Unix(),
	).Err()
	if err != nil {
		return fmt.Errorf("reset window for %s: %w", host, err)
	}
	return nil
}

// --- per-host markers ---

func (s *RedisState) Markers(ctx context.Context, host string) (map[MarkerKind]time.Duration, error) {
	if host == "" {
		return map[MarkerKind]time.Duration{}, nil
	}

	// One pipeline for all three. This is the single most-called method in the
	// package and it runs before every fetch, so three round trips here would be
	// three round trips per URL for a decision that usually reads "nothing set".
	pipe := s.conn.Pipeline()
	ttls := make([]*redis.DurationCmd, len(AllMarkers))
	for i, kind := range AllMarkers {
		ttls[i] = pipe.PTTL(ctx, s.keys.HostMarker(host, kind))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// A pipelined error is only reported when at least one command failed, so
		// check each result rather than trusting Exec's nil.
		return nil, fmt.Errorf("read markers for %s: %w", host, err)
	}

	out := make(map[MarkerKind]time.Duration, len(AllMarkers))
	for i, kind := range AllMarkers {
		ttl, err := ttls[i].Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return nil, fmt.Errorf("read %s marker for %s: %w", kind, host, err)
		}
		// PTTL answers -2 for a key that does not exist and -1 for one with no
		// expiry. Neither is a marker we can act on, and both mean "not set".
		if ttl <= 0 {
			continue
		}
		out[kind] = ttl
	}
	return out, nil
}

func (s *RedisState) SetMarker(ctx context.Context, host string, kind MarkerKind, ttl time.Duration) error {
	if host == "" {
		return nil
	}
	if _, ok := kind.suffix(); !ok {
		return fmt.Errorf("set marker: unknown kind %d", kind)
	}
	if ttl <= 0 {
		// A non-positive TTL would make the key vanish immediately, which reads
		// as "not set" and turns a decision into a no-op. Clearing is the honest
		// interpretation of a caller asking for no time.
		return s.ClearMarker(ctx, host, kind)
	}

	// SET ... NX EX, not SET EX. Twenty workers can all decide a host is dead in
	// the same second; with a plain SET each would push the expiry out to its own
	// "now plus ttl", and the last one to arrive would extend the deadline to a
	// time measured from whenever it happened to run. The marker would then
	// expire well after the backoff schedule predicted, and a host that recovers
	// would stay dark for longer than the policy says it should.
	//
	// The cost of NX is that a *rising* backoff does not lengthen an existing
	// marker: the first failure's 60s stands even after the third failure asks for
	// 240s. That is the conservative direction -- re-probing a host a little early
	// costs one failed request, while probing a dead one late costs the crawl a
	// whole cooldown cycle of nothing.
	err := s.conn.SetArgs(ctx, s.keys.HostMarker(host, kind), 1, redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Err()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, redis.Nil):
		// The marker already exists and NX declined to touch it. That is the
		// outcome we asked for, not a failure: go-redis reports an unsatisfied
		// SET NX as redis.Nil, and treating that as an error would make every
		// second worker to notice a dead host log a spurious warning.
		return nil
	default:
		return fmt.Errorf("set %s marker for %s: %w", kind, host, err)
	}
}

func (s *RedisState) ClearMarker(ctx context.Context, host string, kind MarkerKind) error {
	if host == "" {
		return nil
	}
	if _, ok := kind.suffix(); !ok {
		return fmt.Errorf("clear marker: unknown kind %d", kind)
	}
	if err := s.conn.Del(ctx, s.keys.HostMarker(host, kind)).Err(); err != nil {
		return fmt.Errorf("clear %s marker for %s: %w", kind, host, err)
	}
	return nil
}

// --- observability ---

func (s *RedisState) CountReason(ctx context.Context, host string, reason Reason) error {
	if host == "" || reason == "" {
		return nil
	}
	err := s.conn.HIncrBy(ctx, s.keys.Stats(host), string(reason), 1).Err()
	if err != nil {
		return fmt.Errorf("count %s for %s: %w", reason, host, err)
	}
	return nil
}

// --- helpers ---

// atoiDefault parses a hash field, returning def for anything unparseable. The
// state hash is written by several code paths and read after a restart, so a
// field that is missing, empty or was corrupted by hand must degrade to a
// default rather than fail the read.
func atoiDefault(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

func parseUnix(s string) time.Time {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
