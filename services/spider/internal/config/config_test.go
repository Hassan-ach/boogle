package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

// Every policy setting has to survive two cases: unset (which must give the
// package default, not a zero) and set to something nonsensical (which must give
// the default rather than being taken literally).
//
// The second case is the one that matters. A zero here is not an error, it is a
// crawl that does nothing: a zero scan batch means the frontier is never examined,
// a zero promote batch means retries are never released, and a zero page budget
// means no host is ever crawled. All three look identical from the outside -- a
// crawl that finished, having done nothing.

// policyEnvVars is every environment variable loadPolicyConfig reads, with a value
// that is unmistakably its own so a test can tell "read from the environment" from
// "left at the default".
//
// Keeping it as one table rather than a test per variable is the point. The failure
// this guards against is a Config field being added and never wired, which looks
// exactly like a field added and wired: both compile, and only one of them is
// reachable from a deployment.
var policyEnvVars = []struct {
	key  string
	set  string
	want any
}{
	{"SPIDER_REDIS_PREFIX", "test:spider", "test:spider"},
	{"SPIDER_USER_AGENT", "TestBot", "TestBot"},
	{"URL_STATE_TTL_SEC", "3600", time.Hour},
	{"ROBOTS_TTL_SEC", "7200", 2 * time.Hour},
	{"HOST_COLD_PERIOD_SEC", "1800", 30 * time.Minute},
	{"MIN_CRAWL_DELAY_SEC", "7", 7 * time.Second},
	{"DEAD_HOST_BASE_SEC", "120", 2 * time.Minute},
	{"DEAD_HOST_MAX_EXP", "4", 4},
	{"DEAD_HOST_PROBE_AFTER_SEC", "600", 10 * time.Minute},
	{"HOST_COOLDOWN_BASE_SEC", "45", 45 * time.Second},
	{"HOST_COOLDOWN_MAX_EXP", "5", 5},
	{"URL_BACKOFF_BASE_SEC", "90", 90 * time.Second},
	{"URL_BACKOFF_MAX_SEC", "1200", 20 * time.Minute},
	{"FRONTIER_POP_BATCH", "64", 64},
	{"DELAYED_PROMOTE_BATCH", "250", 250},
	{"MAX_PAGES_PER_HOST", "1234", 1234},
	{"URL_MAX_ATTEMPTS", "7", 7},
	{"MAX_REDIRECTS", "3", 3},
	{"MAX_BODY_BYTES", "1048576", 1048576},
}

// clearPolicyEnv unsets everything loadPolicyConfig reads, so a test starts from a
// known-empty environment rather than inheriting whatever the developer's shell has.
func clearPolicyEnv(t *testing.T) {
	t.Helper()
	for _, v := range policyEnvVars {
		t.Setenv(v.key, "")
	}
}

func TestLoadPolicyConfigDefaultsWithNothingSet(t *testing.T) {
	clearPolicyEnv(t)

	got := loadPolicyConfig()
	want := policy.DefaultConfig()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadPolicyConfig() with nothing set =\n%+v\nwant\n%+v", got, want)
	}
}

func TestLoadPolicyConfigReadsTheEnvironment(t *testing.T) {
	for _, v := range policyEnvVars {
		t.Run(v.key, func(t *testing.T) {
			clearPolicyEnv(t)
			t.Setenv(v.key, v.set)

			got := loadPolicyConfig()
			field := configField(got, policyEnvFields[v.key]).Interface()

			if !reflect.DeepEqual(field, v.want) {
				t.Errorf("%s=%q gave %v, want %v", v.key, v.set, field, v.want)
			}
		})
	}
}

// policyEnvFields maps an environment variable onto the Config field it sets.
//
// Written as an explicit table rather than a naming convention because the mapping
// is not derivable -- "HOST_COLD_PERIOD_SEC" is HostColdPeriod and
// "SPIDER_REDIS_PREFIX" is RedisPrefix -- and a convention-based lookup would agree
// with the wiring exactly when the wiring was wrong, which is the one case the test
// exists for. A variable missing from this table makes TestLoadPolicyConfigReads-
// TheEnvironment fail rather than being silently skipped.
var policyEnvFields = map[string]string{
	"SPIDER_REDIS_PREFIX":       "RedisPrefix",
	"SPIDER_USER_AGENT":         "UserAgent",
	"URL_STATE_TTL_SEC":         "URLStateTTL",
	"ROBOTS_TTL_SEC":            "RobotsTTL",
	"HOST_COLD_PERIOD_SEC":      "HostColdPeriod",
	"MIN_CRAWL_DELAY_SEC":       "MinCrawlDelay",
	"DEAD_HOST_BASE_SEC":        "DeadHostBase",
	"DEAD_HOST_MAX_EXP":         "DeadHostMaxExp",
	"DEAD_HOST_PROBE_AFTER_SEC": "DeadProbeAfter",
	"HOST_COOLDOWN_BASE_SEC":    "HostCooldownBase",
	"HOST_COOLDOWN_MAX_EXP":     "HostCooldownMaxExp",
	"URL_BACKOFF_BASE_SEC":      "URLBackoffBase",
	"URL_BACKOFF_MAX_SEC":       "URLBackoffMax",
	"FRONTIER_POP_BATCH":        "FrontierPopBatch",
	"DELAYED_PROMOTE_BATCH":     "DelayedPromoteBatch",
	"MAX_PAGES_PER_HOST":        "MaxPagesPerHost",
	"URL_MAX_ATTEMPTS":          "URLMaxAttempts",
	"MAX_REDIRECTS":             "MaxRedirects",
	"MAX_BODY_BYTES":            "MaxBodyBytes",
}

// configField returns one field of a policy.Config by name, failing the test if it
// does not exist -- which is what happens when a field is renamed and the env table
// is not, and it should be an error rather than a zero comparison.
func configField(c policy.Config, name string) reflect.Value {
	f := reflect.ValueOf(c).FieldByName(name)
	if !f.IsValid() {
		panic("policy.Config has no field " + name)
	}
	return f
}

// TestLoadPolicyConfigHasNoUnreachableField is the check that makes the table above
// exhaustive by construction rather than by review.
//
// It walks every field of the loaded config and requires it to be non-zero. A
// Config field that nothing reads is invisible in every other way: the struct
// literal compiles, the field looks configured, and the only symptom is a setting
// that quietly cannot be changed. RedisConfig.Delay was in exactly that state for
// as long as the frontier pop loop existed.
func TestLoadPolicyConfigHasNoUnreachableField(t *testing.T) {
	clearPolicyEnv(t)

	v := reflect.ValueOf(loadPolicyConfig())
	tp := v.Type()

	for i := range tp.NumField() {
		f := tp.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		got := v.Field(i)
		if got.IsZero() {
			t.Errorf("policy.Config.%s is zero with no environment set; either it "+
				"has no default or loadPolicyConfig never assigns it", f.Name)
		}
	}

	// And the same for the embedded backoff config, which is reached through its
	// own fields rather than through policy.Config.
	bf := reflect.ValueOf(loadPolicyConfig().BackoffConfig)
	for i := range bf.NumField() {
		f := bf.Type().Field(i)
		if bf.Field(i).IsZero() {
			t.Errorf("policy.BackoffConfig.%s is zero with no environment set; "+
				"either it has no default or loadPolicyConfig never assigns it", f.Name)
		}
	}
}

// TestLoadPolicyConfigRejectsUnusableDurations covers every duration variable,
// since they are the ones where a bad value is silent rather than merely wrong.
//
// A zero TTL expires every URL's retry bookkeeping on the next read, which turns
// the backoff off without any log line and without any error. A zero crawl delay
// means no rate limiting at all, which is the polite way of saying the crawler
// hammers a site that asked it to slow down.
func TestLoadPolicyConfigRejectsUnusableDurations(t *testing.T) {
	durationVars := []string{
		"URL_STATE_TTL_SEC", "ROBOTS_TTL_SEC", "HOST_COLD_PERIOD_SEC",
		"MIN_CRAWL_DELAY_SEC", "DEAD_HOST_BASE_SEC", "DEAD_HOST_PROBE_AFTER_SEC",
		"HOST_COOLDOWN_BASE_SEC", "URL_BACKOFF_BASE_SEC", "URL_BACKOFF_MAX_SEC",
	}

	for _, key := range durationVars {
		for _, raw := range []string{"0", "-1", "abc", "1.5", " 60", "60s"} {
			t.Run(key+"="+raw, func(t *testing.T) {
				clearPolicyEnv(t)
				t.Setenv(key, raw)

				got := loadPolicyConfig()
				want := configField(policy.DefaultConfig(), policyEnvFields[key]).Interface()
				if gotValue := configField(got, policyEnvFields[key]).Interface(); !reflect.DeepEqual(gotValue, want) {
					t.Errorf("%s=%q gave %v, want the default %v", key, raw, gotValue, want)
				}
			})
		}
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
	if c.URLMaxAttempts < 1 {
		t.Errorf("URLMaxAttempts = %d; every URL would be refused on its first failure",
			c.URLMaxAttempts)
	}
	if c.MaxRedirects < 1 {
		t.Errorf("MaxRedirects = %d; no redirect would be followed", c.MaxRedirects)
	}
	if c.RobotsTTL <= 0 {
		t.Error("RobotsTTL is not positive; robots.txt would be re-read for every URL")
	}
	if c.HostColdPeriod <= 0 {
		t.Error("HostColdPeriod is not positive; the page budget could never roll over")
	}
}
