package policy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// The two frontier scripts.
//
// Both are Lua for the same reason: each one has to read and write across two
// keys without another client interleaving. PopFrontier reads the visited set
// for an entry it is deciding about, and PromoteDelayed moves a URL from one
// sorted set to another. A round trip in the middle of either would let a
// concurrent worker duplicate a URL into the frontier or lose one between the
// read and the write.
//
// The script bodies are constants rather than built with fmt, so Redis can
// fingerprint them and reuse the cached copy instead of re-parsing Lua on every
// call. Both run before every fetch, so that difference is measurable.

// popFrontierScript takes the highest-scoring unvisited URL.
//
// It pops up to `budget` entries, returning the first that is not in the visited
// set. Entries that are already visited are discarded, which is correct: they
// are in the frontier because something added them without checking, and leaving
// them there would mean re-examining them on every future pop.
//
// The return is {url, found, exhausted, skipped}.
//
// `exhausted` is what makes the loop above it terminate. Without it, a caller
// cannot distinguish "the frontier is empty, go idle" from "I used my whole
// scan budget on visited entries, there may be more" -- and both look identical
// to a found=0 answer. Confusing them is what produced the original unbounded
// loop, in the other direction: treating budget exhaustion as emptiness would
// make the crawl go idle with unvisited URLs still sitting in the frontier, and
// those URLs would not come back until something else happened to enqueue them.
//
// Note the asymmetry that makes this bounded: each iteration pops one entry, so
// `budget` iterations can consume at most `budget` entries. A frontier of
// entirely-visited entries is drained in ceil(n/budget) calls and then reports
// exhausted. It cannot loop forever.
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

// PopFrontier takes the highest-priority URL that has not been crawled.
//
// See PopResult for why the answer reports more than a URL and a bool.
func (s *RedisState) PopFrontier(ctx context.Context, budget int) (PopResult, error) {
	// A non-positive budget means "do nothing" if taken literally, which would
	// silently stop the entire crawl -- and FRONTIER_POP_BATCH is read from the
	// environment, so a zero is a realistic input rather than a hypothetical one.
	// It resolves to the package default instead: a nonsensical value degrades to the
	// documented behaviour, not to a degenerate one.
	//
	// MemoryState resolves it identically, because a fake that disagrees with the real
	// implementation on this would let a test pass while production took a different
	// path. That is not hypothetical: the two did disagree, and the fake was the one
	// that looked right.
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

// parsePopResult decodes the script's {url, found, exhausted, skipped} array.
//
// It is a separate function so the shape can be tested without a Redis. The two
// checks are the load-bearing part and neither is defensive programming for its
// own sake:
//
//   - A wrong number of fields means the script and this parser have drifted.
//     Reading the fields anyway would turn a schema mismatch into URLs quietly
//     going missing or a crawl that never terminates -- both of which look like
//     "the crawl is doing nothing" from the outside.
//
//   - A found flag with an empty URL, or a URL with no found flag, is a
//     contradiction. Passing it through would hand the caller an empty string to
//     crawl, which parses as a relative URL and resolves to the crawler's own
//     host.
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

// promoteDelayedScript moves URLs whose retry has come due into the frontier.
//
// Both operations have to be atomic together. Moving first and removing second
// would let a concurrent call promote the same URL twice; removing first and
// moving second would lose it entirely on a crash between the two.
//
// ZADD NX matters here. A retried URL may also have been discovered again in the
// meantime, and NX preserves the higher score it earned from those inlinks. A
// plain ZADD would overwrite it with 0 and demote a page that thirty other pages
// link to, just because it had one flaky fetch.
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

// PromoteDelayed moves URLs whose retry has come due into the frontier.
//
// There is no timer anywhere in this design. A parked URL becomes available
// because the crawl happens to look, which means a URL nobody promotes simply
// never comes back -- so promotion runs at the top of every crawl rather than
// being a background job that could be missed.
func (s *RedisState) PromoteDelayed(ctx context.Context, now time.Time, batch int) (int, error) {
	if batch < 1 {
		batch = defaultPromoteBatch
	}

	// Truncated to whole seconds because the score is a unix second, and a
	// sub-second offset would make a URL due in 30.4s eligible at 30.0s.
	nowUnix := now.Unix()

	n, err := promoteDelayedScript.Run(ctx, s.conn,
		[]string{s.keys.Delayed(), s.keys.Frontier()},
		nowUnix,
		batch,
	).Int()
	if err != nil {
		return 0, fmt.Errorf("promote delayed: %w", err)
	}
	// The script cannot return a negative count, so this is a contract check
	// rather than a validation of real data.
	if n < 0 {
		return 0, fmt.Errorf("promote delayed: script returned a negative count %d", n)
	}
	return n, nil
}

// PromoteAllDue moves every due URL into the frontier, looping while each pass
// comes back full.
//
// The batch cap is a Lua 5.1 limitation rather than a design choice: unpack is
// bounded by the Lua stack, and Redis embeds 5.1. The Go caller looping is both
// safer and faster than raising the cap, since each pass is one round trip over
// a bounded set.
func (s *RedisState) PromoteAllDue(ctx context.Context, now time.Time) (int, error) {
	batch := defaultPromoteBatch
	total := 0

	for {
		n, err := s.PromoteDelayed(ctx, now, batch)
		if err != nil {
			return total, err
		}
		total += n

		// A short pass means the delayed set has nothing more that is due.
		if n < batch {
			return total, nil
		}
		// A full pass with nothing new on the next one would spin, but the
		// script removes what it promotes, so the next pass cannot return the
		// same count twice. The guard is here for the case where something else
		// is adding to the delayed set faster than we drain it.
		if total > maxPromotionsPerCall {
			return total, fmt.Errorf("promote delayed: stopped after %d promotions, the delayed set is growing faster than it drains", total)
		}
	}
}

// maxPromotionsPerCall bounds PromoteAllDue. It is not a limit on how much may be
// promoted -- it is a limit on how long one call may run, and it exists only so
// that a writer outpacing the drainer is a loud error rather than a hang.
const maxPromotionsPerCall = 1_000_000

// Defaults for the two batch sizes, used when the configuration supplies none.
//
// These are not only defaults. A non-positive batch supplied by a caller resolves
// to them, on the reasoning that a nonsensical configuration value should degrade
// to the documented behaviour rather than to a degenerate one: a zero scan batch
// stalls the crawl, and a zero promote batch parks every retry forever, and both
// look exactly like a crawl that has finished.
const (
	defaultPopBatch     = 16
	defaultPromoteBatch = 100
)

// toString and toInt convert the untyped values a Lua array arrives as.
//
// Redis returns every element of a Lua table as a bulk string, so a count comes
// back as []byte("1") rather than an int. A type assertion that assumed
// otherwise would panic on the very first pop, and a fmt.Sprintf("%v") fallback
// would turn a count into a string like "[] uint8 {...}" -- which parses as 0,
// so every pop would report "not found" and the crawl would never run.
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
