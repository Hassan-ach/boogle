package config

import (
	"testing"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

// Every policy setting has to survive two cases: unset (which must give the
// package default, not a zero) and set to something nonsensical (which must give
// the default rather than being taken literally).
//
// The second case is the one that matters. A zero here is not an error, it is a
// crawl that does nothing: a zero scan batch means the frontier is never
// examined, a zero promote batch means retries are never released, and a zero
// page budget means no host is ever crawled. All three look identical from the
// outside -- a crawl that finished, having done nothing.

func TestLoadPolicyConfigDefaultsWithNothingSet(t *testing.T) {
	// A var rather than a literal, so it can be given to t.Setenv.
	for _, key := range []string{
		"SPIDER_REDIS_PREFIX", "URL_STATE_TTL_SEC",
		"FRONTIER_POP_BATCH", "DELAYED_PROMOTE_BATCH",
	} {
		t.Setenv(key, "")
	}

	got := loadPolicyConfig()
	want := policy.DefaultConfig()

	if got.RedisPrefix != want.RedisPrefix {
		t.Errorf("RedisPrefix = %q, want %q", got.RedisPrefix, want.RedisPrefix)
	}
	if got.URLStateTTL != want.URLStateTTL {
		t.Errorf("URLStateTTL = %s, want %s", got.URLStateTTL, want.URLStateTTL)
	}
	if got.FrontierPopBatch != want.FrontierPopBatch {
		t.Errorf("FrontierPopBatch = %d, want %d", got.FrontierPopBatch, want.FrontierPopBatch)
	}
	if got.DelayedPromoteBatch != want.DelayedPromoteBatch {
		t.Errorf("DelayedPromoteBatch = %d, want %d", got.DelayedPromoteBatch, want.DelayedPromoteBatch)
	}
}

func TestLoadPolicyConfigReadsTheEnvironment(t *testing.T) {
	t.Setenv("SPIDER_REDIS_PREFIX", "test:spider")
	t.Setenv("URL_STATE_TTL_SEC", "3600")
	t.Setenv("FRONTIER_POP_BATCH", "64")
	t.Setenv("DELAYED_PROMOTE_BATCH", "250")

	got := loadPolicyConfig()

	if got.RedisPrefix != "test:spider" {
		t.Errorf("RedisPrefix = %q, want test:spider", got.RedisPrefix)
	}
	if got.URLStateTTL != time.Hour {
		t.Errorf("URLStateTTL = %s, want 1h", got.URLStateTTL)
	}
	if got.FrontierPopBatch != 64 {
		t.Errorf("FrontierPopBatch = %d, want 64", got.FrontierPopBatch)
	}
	if got.DelayedPromoteBatch != 250 {
		t.Errorf("DelayedPromoteBatch = %d, want 250", got.DelayedPromoteBatch)
	}
}

// TestLoadPolicyConfigRejectsUnusableDurations covers the TTL specifically, since
// it is the one setting where a bad value is silent rather than merely wrong. A
// zero TTL expires every URL's retry bookkeeping on the next read, which turns the
// backoff off without any log line and without any error.
func TestLoadPolicyConfigRejectsUnusableDurations(t *testing.T) {
	for _, raw := range []string{"0", "-1", "abc", "1.5", " 60", "60s"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("URL_STATE_TTL_SEC", raw)
			got := loadPolicyConfig()
			if got.URLStateTTL != policy.DefaultConfig().URLStateTTL {
				t.Errorf("URL_STATE_TTL_SEC=%q gave %s, want the default %s",
					raw, got.URLStateTTL, policy.DefaultConfig().URLStateTTL)
			}
		})
	}
}

// TestLoadPolicyConfigLeavesTheBackoffIntact guards against a later phase
// overwriting a field it does not read. loadPolicyConfig starts from
// DefaultConfig, so anything not explicitly loaded keeps the package default --
// which is only true if nothing writes over the whole struct.
func TestLoadPolicyConfigLeavesTheBackoffIntact(t *testing.T) {
	t.Setenv("SPIDER_REDIS_PREFIX", "")
	t.Setenv("URL_STATE_TTL_SEC", "")

	got := loadPolicyConfig()
	want := policy.DefaultConfig()

	if got.BackoffConfig != want.BackoffConfig {
		t.Errorf("BackoffConfig = %+v, want %+v", got.BackoffConfig, want.BackoffConfig)
	}
	if got.MaxPagesPerHost != want.MaxPagesPerHost {
		t.Errorf("MaxPagesPerHost = %d, want %d", got.MaxPagesPerHost, want.MaxPagesPerHost)
	}
	if got.HostColdPeriod != want.HostColdPeriod {
		t.Errorf("HostColdPeriod = %s, want %s", got.HostColdPeriod, want.HostColdPeriod)
	}
	if got.MaxBodyBytes != want.MaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want %d", got.MaxBodyBytes, want.MaxBodyBytes)
	}
}

// TestPolicyConfigDefaultsAreSelfConsistent checks the relationships between the
// defaults rather than the values themselves.
//
// Two of these are not arbitrary. A dead-host base longer than the probe delay
// would expire the marker before the URLs parked behind it came back, so a
// recovered domain would be re-probed and its retries released against a host
// still marked dead. And a URL backoff base longer than the maximum is a schedule
// that never grows, which looks like exponential backoff and is not.
func TestPolicyConfigDefaultsAreSelfConsistent(t *testing.T) {
	c := policy.DefaultConfig()

	if c.DeadProbeAfter < c.DeadHostBase {
		t.Errorf("DeadProbeAfter (%s) is shorter than DeadHostBase (%s); the dead "+
			"marker would expire before the urls parked behind it come back",
			c.DeadProbeAfter, c.DeadHostBase)
	}
	if c.URLBackoffBase >= c.URLBackoffMax {
		t.Errorf("URLBackoffBase (%s) is not below URLBackoffMax (%s); the schedule "+
			"never grows", c.URLBackoffBase, c.URLBackoffMax)
	}
	if c.DeadHostMaxExp < 1 {
		t.Errorf("DeadHostMaxExp = %d, want at least 1", c.DeadHostMaxExp)
	}
	if c.DeadHostMaxExp > 20 {
		// 2^20 seconds is over twelve days, which for a crawl that is supposed to
		// re-probe a domain is indistinguishable from abandoning it.
		t.Errorf("DeadHostMaxExp = %d; a dead host would be parked for over a week",
			c.DeadHostMaxExp)
	}
	if c.MinCrawlDelay <= 0 {
		t.Error("MinCrawlDelay is not positive; a robots.txt asking for no delay " +
			"must not be obeyed literally")
	}
}
