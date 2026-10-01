package policy

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisState is the durable State implementation. Everything that must be
// atomic across several keys runs as a Lua script rather than a pipeline,
// because a pipeline is not a transaction and can interleave with other clients.
type RedisState struct {
	conn   *redis.Client
	keys   Keyspace
	urlTTL time.Duration
}

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

func (s *RedisState) Keys() Keyspace { return s.keys }

func (s *RedisState) Close() error { return s.conn.Close() }

func (s *RedisState) MarkVisited(ctx context.Context, urls ...string) error {
	if len(urls) == 0 {
		return nil
	}
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

func (s *RedisState) Enqueue(ctx context.Context, urls ...string) error {
	if len(urls) == 0 {
		return nil
	}
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

func (s *RedisState) URLState(ctx context.Context, url string) (URLState, error) {
	fields, err := s.conn.HGetAll(ctx, s.keys.URLState(url)).Result()
	if err != nil {
		return URLState{}, fmt.Errorf("read url state: %w", err)
	}
	if len(fields) == 0 {
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

	pipe := s.conn.Pipeline()
	incr := pipe.HIncrBy(ctx, key, fieldAttempts, 1)
	pipe.Expire(ctx, key, s.urlTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("bump attempts: %w", err)
	}
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

func (s *RedisState) HostState(ctx context.Context, host string) (HostState, error) {
	fields, err := s.conn.HGetAll(ctx, s.keys.HostState(host)).Result()
	if err != nil {
		return HostState{}, fmt.Errorf("read host state: %w", err)
	}
	st := HostState{Name: host}
	if len(fields) == 0 {
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

func (s *RedisState) SaveHostState(ctx context.Context, host string, st HostState) error {
	if host == "" {
		return errors.New("save host state: empty host")
	}

	values := map[string]any{
		fieldMaxPages:   st.MaxPages,
		fieldCrawlDelay: st.CrawlDelay.Milliseconds(),
	}
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

func (s *RedisState) Markers(ctx context.Context, host string) (map[MarkerKind]time.Duration, error) {
	if host == "" {
		return map[MarkerKind]time.Duration{}, nil
	}

	pipe := s.conn.Pipeline()
	ttls := make([]*redis.DurationCmd, len(AllMarkers))
	for i, kind := range AllMarkers {
		ttls[i] = pipe.PTTL(ctx, s.keys.HostMarker(host, kind))
	}
	if _, err := pipe.Exec(ctx); err != nil {
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
		return s.ClearMarker(ctx, host, kind)
	}

	err := s.conn.SetArgs(ctx, s.keys.HostMarker(host, kind), 1, redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Err()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, redis.Nil):
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

var popFrontierScript = redis.NewScript(`
local frontier = KEYS[1]
local visited = KEYS[2]
local budget = tonumber(ARGV[1])

if budget == nil or budget < 1 then
  budget = 1
end

local skipped = 0

for i = 1, budget do
  local entry = redis.call('zpopmax', frontier, 1)

  if #entry == 0 then
    -- The frontier is empty. Report it as exhausted so the caller stops
    -- asking, having skipped whatever stale entries preceded this.
    return {'', 0, 1, skipped}
  end

  local url = entry[1]

  -- An empty member is not a URL. It can only get here from a write that did not
  -- go through Enqueue, but the script is the last thing standing between that
  -- and a fetch of "" -- which parses as a relative URL and resolves to the
  -- crawler's own host. Discarded here rather than returned, so one bad member
  -- costs the caller nothing at all: the alternative is a hard error on the hot
  -- path, and a hard error for a member that has already been removed.
  if url == '' then
    skipped = skipped + 1
  elseif redis.call('sismember', visited, url) == 0 then
    -- Found one. Everything skipped so far was genuinely stale, and the
    -- remaining budget is deliberately unused: the caller asked for one URL.
    return {url, 1, 0, skipped}
  else
    skipped = skipped + 1
  end
end

-- Budget exhausted with only visited entries behind it. The frontier may still
-- hold unvisited work further down, so this is explicitly not exhausted.
return {'', 0, 0, skipped}
`)

func (s *RedisState) PopFrontier(ctx context.Context, budget int) (PopResult, error) {
	if budget < 1 {
		budget = defaultPopBatch
	}

	raw, err := popFrontierScript.Run(ctx, s.conn,
		[]string{s.keys.Frontier(), s.keys.Visited()},
		budget,
	).Result()
	if err != nil {
		return PopResult{}, fmt.Errorf("pop frontier: %w", err)
	}

	result, err := parsePopResult(raw)
	if err != nil {
		return PopResult{}, err
	}
	return result, nil
}

func parsePopResult(raw any) (PopResult, error) {
	fields, ok := raw.([]any)
	if !ok || len(fields) != 4 {
		return PopResult{}, fmt.Errorf("pop frontier: script returned %T with %v elements, want a 4-element array",
			raw, len(fields))
	}

	result := PopResult{
		URL:            toString(fields[0]),
		Found:          toInt(fields[1]) != 0,
		Exhausted:      toInt(fields[2]) != 0,
		VisitedSkipped: toInt(fields[3]),
	}

	if result.Found && result.URL == "" {
		return PopResult{}, fmt.Errorf("pop frontier: script reported a URL found with an empty value")
	}
	if !result.Found && result.URL != "" {
		return PopResult{}, fmt.Errorf("pop frontier: script returned %q with found=0", result.URL)
	}

	return result, nil
}

var promoteDelayedScript = redis.NewScript(`
local delayed = KEYS[1]
local frontier = KEYS[2]
local now = tonumber(ARGV[1])
local batch = tonumber(ARGV[2])

if batch == nil or batch < 1 then
  batch = 1
end

local due = redis.call('zrangebyscore', delayed, '-inf', now, 'limit', 0, batch)

if #due == 0 then
  return 0
end

-- Retries enter at score 0. A fresh discovery climbs by ZINCRBY, so a backlog
-- of failing URLs cannot starve real new work, while a URL that has been found
-- again keeps whatever score those inlinks gave it.
for i = 1, #due do
  redis.call('zadd', frontier, 'NX', 0, due[i])
end

redis.call('zrem', delayed, unpack(due))

return #due
`)

func (s *RedisState) PromoteDelayed(ctx context.Context, now time.Time, batch int) (int, error) {
	if batch < 1 {
		batch = defaultPromoteBatch
	}

	nowUnix := now.Unix()

	n, err := promoteDelayedScript.Run(ctx, s.conn,
		[]string{s.keys.Delayed(), s.keys.Frontier()},
		nowUnix,
		batch,
	).Int()
	if err != nil {
		return 0, fmt.Errorf("promote delayed: %w", err)
	}
	if n < 0 {
		return 0, fmt.Errorf("promote delayed: script returned a negative count %d", n)
	}
	return n, nil
}

func (s *RedisState) PromoteAllDue(ctx context.Context, now time.Time) (int, error) {
	batch := defaultPromoteBatch
	total := 0

	for {
		n, err := s.PromoteDelayed(ctx, now, batch)
		if err != nil {
			return total, err
		}
		total += n

		if n < batch {
			return total, nil
		}
		if total > maxPromotionsPerCall {
			return total, fmt.Errorf("promote delayed: stopped after %d promotions, the delayed set is growing faster than it drains", total)
		}
	}
}

// Caps how many due entries one PromoteDelayed call may promote, so a large
// backlog cannot hold Redis inside the Lua loop for an unbounded time.
const maxPromotionsPerCall = 1_000_000

const (
	defaultPopBatch     = 16
	defaultPromoteBatch = 100
)

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func toInt(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	case []byte:
		n, _ := strconv.Atoi(string(t))
		return n
	default:
		return 0
	}
}

// DefaultRedisPrefix namespaces every key this package owns.
const DefaultRedisPrefix = "boogle:spider"

// Keyspace builds every Redis key the policy manager uses. It is a value type
// with no behaviour beyond naming, so a test can construct one and assert
// against real key strings without a Redis anywhere in sight.
type Keyspace struct {
	prefix string
}

// NewKeyspace returns a Keyspace for the given prefix, normalised. A trailing
// colon is tolerated because SPIDER_REDIS_PREFIX is set by hand, and "boogle:
// spider:" is an easy thing to type.
func NewKeyspace(prefix string) Keyspace {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.TrimRight(prefix, ":")
	if prefix == "" {
		prefix = DefaultRedisPrefix
	}
	return Keyspace{prefix: prefix}
}

func (k Keyspace) Prefix() string { return k.prefix }

// Every key below carries a hash tag: the {braced} segment Redis Cluster uses to
// pick a slot. It keeps a host's state hash and its three TTL markers in one
// slot, so a multi-key script stays legal if this cache is ever clustered. The
// tag wraps the whole key, host included, so what is shared is the host's
// identity and not a prefix common to every host.
func (k Keyspace) Frontier() string { return "{" + k.prefix + ":frontier}" }

func (k Keyspace) Delayed() string { return "{" + k.prefix + ":delayed}" }

func (k Keyspace) Visited() string { return "{" + k.prefix + ":visited}" }

func (k Keyspace) hostTag(host string) string {
	return "{" + k.prefix + ":host:" + sanitizeTag(host) + "}"
}

func (k Keyspace) HostState(host string) string { return k.hostTag(host) }

func (k Keyspace) HostMarker(host string, kind MarkerKind) string {
	suffix, ok := kind.suffix()
	if !ok {
		return "{" + k.prefix + ":host:" + sanitizeTag(host) + "}:invalid-marker"
	}
	return k.hostTag(host) + ":" + suffix
}

// URLs are SHA-1 hashed because they are unbounded; hosts are only sanitized
// because they are short and stay readable in redis-cli.
func (k Keyspace) URLState(url string) string {
	sum := sha1.Sum([]byte(url))
	return "{" + k.prefix + ":url:" + hex.EncodeToString(sum[:]) + "}"
}

func (k Keyspace) Stats(host string) string {
	return "{" + k.prefix + ":stats:" + sanitizeTag(host) + "}"
}

const (
	fieldCrawlDelay        = "crawl_delay_ms"
	fieldMaxPages          = "max_pages"
	fieldPagesCrawled      = "pages_crawled"
	fieldWindowStart       = "window_started_at"
	fieldFailures          = "consec_failures"
	fieldStatus            = "status"
	fieldRobotsAt          = "robots_at"
	fieldFirstSeen         = "first_seen"
	fieldLastSuccess       = "last_success"
	fieldAllow             = "allow"
	fieldDisallow          = "disallow"
	fieldSitemaps          = "sitemaps"
	fieldSitemapsClaimedAt = "sitemaps_claimed_at"
)

const (
	fieldAttempts   = "attempts"
	fieldLastStatus = "last_status"
	fieldLastError  = "last_error"
	fieldEnqueuedAt = "enqueued_at"
	fieldURL        = "url"
)

func (kind MarkerKind) suffix() (string, bool) {
	switch kind {
	case MarkerCooldown:
		return "cooldown", true
	case MarkerDead:
		return "dead", true
	case MarkerCold:
		return "cold", true
	}
	return "", false
}

func (kind MarkerKind) String() string {
	if s, ok := kind.suffix(); ok {
		return s
	}
	return "invalid"
}

func sanitizeTag(s string) string {
	if !strings.ContainsAny(s, "{}") {
		return s
	}
	return strings.NewReplacer("{", "(", "}", ")").Replace(s)
}
