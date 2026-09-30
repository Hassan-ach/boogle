package policy

import "time"

// The three retry schedules. They are separate on purpose: they have different
// bases, different growth rates and different consequences, and collapsing them
// into one "backoff" function is what made the original design hard to reason
// about.
//
// Every function here is total -- negative counters, zero bases and absurd
// inputs all produce a usable duration rather than a panic or a negative
// interval. These run on the hot path with values read from Redis, and a
// corrupt counter must degrade into "retry soon", never into "retry in the past"
// (which would re-queue forever) or "retry in year 3000" (which strands the URL
// silently).

// DeadHostTTL is how long a host is skipped after a connection-level failure --
// DNS, dial or TLS. No HTTP response was ever received, so the host is presumed
// gone and every URL on it is answered from Redis instead of the network.
//
// attempts is the *consecutive* failure count, so a host that recovers and later
// dies again starts from the first TTL rather than the largest one.
func (c BackoffConfig) DeadHostTTL(attempts int) time.Duration {
	return scale(c.DeadHostBase, attempts, c.DeadHostMaxExp)
}

// HostCooldown is how long a host that is answering but failing us is left
// alone. 5xx and 429 land here: the server is reachable, so this is not a dead
// host, but hammering it is how an IP gets banned.
//
// crawlDelay is whatever robots.txt asked for, in seconds. It acts as a floor
// on the first cooldown and is ignored once the exponential term exceeds it --
// a server asking for 5s between requests should not have that overridden by a
// 2s first failure, but a 20s backoff should not be shortened to 5s either.
func (c BackoffConfig) HostCooldown(attempts int, crawlDelay time.Duration) time.Duration {
	floor := c.HostCooldownBase
	if crawlDelay > floor {
		floor = crawlDelay
	}
	return scale(floor, attempts, c.HostCooldownMaxExp)
}

// URLBackoff is the delay before retrying a single URL. This is the narrowest
// schedule: one flaky page, with the host itself unaffected.
//
// attempt is 0 for the first retry, so the sequence is base, 2*base, 4*base...
// clamped to URLBackoffMax so one page cannot drift out to an hour and be
// assumed abandoned by an operator rather than by the policy.
func (c BackoffConfig) URLBackoff(attempt int) time.Duration {
	d := scale(c.URLBackoffBase, attempt, -1)
	if c.URLBackoffMax > 0 && d > c.URLBackoffMax {
		return c.URLBackoffMax
	}
	return d
}

// scale computes base * 2^min(n, maxExp).
//
// maxExp < 0 means uncapped, which only URLBackoff uses -- it has its own clamp
// to a different field (URLBackoffMax) and shares no exponent cap with the host
// schedules, since a page-level retry after 9 failures is legitimately worth
// 15 minutes while a host is already dead by then.
//
// The doubling is done with an explicit bound rather than math.Pow so the
// arithmetic stays in integers: base is a time.Duration, and a float multiply
// large enough to exceed ~2^62 nanoseconds loses precision in a way that would
// only show up as a nonsense TTL weeks into a long-running crawl.
func scale(base time.Duration, n, maxExp int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if n < 0 {
		n = 0
	}
	if maxExp >= 0 && n > maxExp {
		n = maxExp
	}
	// 62 is the largest left shift of a time.Duration that cannot overflow
	// int64. Clamping there means the worst case is a very long, but finite and
	// representable, interval -- and every caller caps it further anyway.
	const maxShift = 62
	if n > maxShift {
		n = maxShift
	}
	d := base << uint(n)
	if d <= 0 {
		// Shift overflowed into a non-positive duration.
		return time.Duration(1<<62 - 1)
	}
	return d
}
