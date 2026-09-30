package policy

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// Redis key construction.
//
// Every key carries a hash tag, the {braced} part Redis Cluster uses to decide
// which slot a key belongs to. That is what keeps a host's four keys -- its
// state hash and its three TTL markers -- in the same slot, so a multi-key
// pipeline or script touching all of them stays legal if this cache is ever
// clustered. The tags cost nothing today on a single node.
//
// The tag wraps the *whole* key including the host, which is the point: it is
// the host identity that all four keys share, not a common prefix that would be
// shared by every host.

// DefaultRedisPrefix namespaces every key this package owns.
const DefaultRedisPrefix = "boogle:spider"

// Keyspace builds every key the policy manager uses. It is a value type with no
// behaviour beyond naming, so a test can construct one and assert against real
// key strings without a Redis anywhere in sight.
type Keyspace struct {
	prefix string
}

// NewKeyspace returns a Keyspace for the given prefix, normalised.
//
// A trailing colon is tolerated because SPIDER_REDIS_PREFIX is set by hand and
// "boogle:spider:" is an easy thing to type. Braces are rejected outright: they
// would silently redefine the hash tag and scatter a host's keys across slots.
func NewKeyspace(prefix string) Keyspace {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.TrimRight(prefix, ":")
	// Re-checked after trimming, not before: a prefix of ":" or "::" trims away
	// to nothing, and an empty prefix would build keys like "{:frontier}" -- a
	// broken hash tag in a namespace that silently collides with anything else
	// using the empty prefix.
	if prefix == "" {
		prefix = DefaultRedisPrefix
	}
	return Keyspace{prefix: prefix}
}

// Prefix returns the normalised namespace, without a trailing separator.
func (k Keyspace) Prefix() string { return k.prefix }

// Frontier is the ZSET of URLs available to crawl, scored by inlink priority.
func (k Keyspace) Frontier() string { return "{" + k.prefix + ":frontier}" }

// Delayed is the ZSET of URLs parked until a retry, scored by due unix seconds.
func (k Keyspace) Delayed() string { return "{" + k.prefix + ":delayed}" }

// Visited is the SET of URLs that have been resolved and need not be seen again.
//
// It holds the full URL rather than a hash, because exact-string dedup is the
// correctness property here and a hash would make that a collision question.
// The per-URL state hash below is the one that is hashed, because it is written
// on every attempt and a URL can run to several kilobytes.
func (k Keyspace) Visited() string { return "{" + k.prefix + ":visited}" }

// hostTag is the shared hash tag for every key belonging to one host. The
// marker keys below append their suffix *outside* the braces, which is what
// keeps them in this host's slot.
func (k Keyspace) hostTag(host string) string {
	return "{" + k.prefix + ":host:" + sanitizeTag(host) + "}"
}

// HostState is the hash of per-host policy state: delay, budgets, failure
// counts and the cached robots.txt rules.
func (k Keyspace) HostState(host string) string { return k.hostTag(host) }

// HostMarker is one of the three short-lived per-host markers. Each is a plain
// key with a TTL rather than a field on the state hash, because Redis only
// gained per-field expiry in 7.4 and a cooldown that has to expire cannot be
// modelled as a hash field on older servers.
func (k Keyspace) HostMarker(host string, kind MarkerKind) string {
	suffix, ok := kind.suffix()
	if !ok {
		// An unknown kind is a programming error, not a runtime condition.
		// Returning the state key would let a caller SET over the host's real
		// policy state, so refuse with something that cannot collide.
		return "{" + k.prefix + ":host:" + sanitizeTag(host) + "}:invalid-marker"
	}
	return k.hostTag(host) + ":" + suffix
}

// URLState is the per-URL retry bookkeeping hash: attempt count and the last
// failure seen.
func (k Keyspace) URLState(url string) string {
	sum := sha1.Sum([]byte(url))
	return "{" + k.prefix + ":url:" + hex.EncodeToString(sum[:]) + "}"
}

// Stats is the per-host counter hash. Every refusal records its reason here,
// which is what makes "what is this crawler declining, and how much" a question
// with an answer.
func (k Keyspace) Stats(host string) string {
	return "{" + k.prefix + ":stats:" + sanitizeTag(host) + "}"
}

// Marker names used inside a host's state hash and stats hash.
const (
	fieldCrawlDelay   = "crawl_delay_ms"
	fieldMaxPages     = "max_pages"
	fieldPagesCrawled = "pages_crawled"
	fieldWindowStart  = "window_started_at"
	fieldFailures     = "consec_failures"
	fieldStatus       = "status"
	fieldRobotsAt     = "robots_at"
	fieldFirstSeen    = "first_seen"
	fieldLastSuccess  = "last_success"
	fieldAllow        = "allow"
	fieldDisallow     = "disallow"
)

// URL state hash fields.
const (
	fieldAttempts   = "attempts"
	fieldLastStatus = "last_status"
	fieldLastError  = "last_error"
	fieldEnqueuedAt = "enqueued_at"
	fieldURL        = "url"
)

// markerSuffix maps a MarkerKind to the string that follows the host's hash tag.
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

// sanitizeTag makes a host safe to embed in a hash tag.
//
// Hostnames come from parsed URLs, so braces are not expected, but a host that
// reached here with a brace in it would silently move a key to another slot and
// break the co-location the whole scheme depends on. Substituting is the safe
// response: the key stays deterministic, so the same odd host always maps to
// the same key, and it stays inside its tag.
func sanitizeTag(s string) string {
	if !strings.ContainsAny(s, "{}") {
		return s
	}
	return strings.NewReplacer("{", "(", "}", ")").Replace(s)
}
