package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

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

var deprecatedEnvVars = []string{"REDIS_MAX_RETRY"}

func clearPolicyEnv(t *testing.T) {
	t.Helper()
	for _, v := range policyEnvVars {
		t.Setenv(v.key, "")
	}
	for _, key := range deprecatedEnvVars {
		t.Setenv(key, "")
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

func configField(c policy.Config, name string) reflect.Value {
	f := reflect.ValueOf(c).FieldByName(name)
	if !f.IsValid() {
		panic("policy.Config has no field " + name)
	}
	return f
}

func TestLoadPolicyConfigHasNoUnreachableField(t *testing.T) {
	clearPolicyEnv(t)

	v := reflect.ValueOf(loadPolicyConfig())
	tp := v.Type()

	for i := range tp.NumField() {
		f := tp.Field(i)
		if f.PkgPath != "" {
			continue
		}
		got := v.Field(i)
		if got.IsZero() {
			t.Errorf("policy.Config.%s is zero with no environment set; either it "+
				"has no default or loadPolicyConfig never assigns it", f.Name)
		}
	}

	bf := reflect.ValueOf(loadPolicyConfig().BackoffConfig)
	for i := range bf.NumField() {
		f := bf.Type().Field(i)
		if bf.Field(i).IsZero() {
			t.Errorf("policy.BackoffConfig.%s is zero with no environment set; "+
				"either it has no default or loadPolicyConfig never assigns it", f.Name)
		}
	}
}

func TestTheOldPopBudgetIsStillHonouredWhenTheNewNameIsNot(t *testing.T) {
	clearPolicyEnv(t)
	t.Setenv("REDIS_MAX_RETRY", "7")

	if got := loadPolicyConfig().FrontierPopBatch; got != 7 {
		t.Errorf("with only REDIS_MAX_RETRY=7 set, FrontierPopBatch = %d, want 7; a "+
			"rename that changes behaviour the day it ships is not a rename", got)
	}

	t.Setenv("FRONTIER_POP_BATCH", "3")
	if got := loadPolicyConfig().FrontierPopBatch; got != 3 {
		t.Errorf("with both set, FrontierPopBatch = %d, want 3 from FRONTIER_POP_BATCH", got)
	}
}

func TestRedisConfigHasNoDeadFields(t *testing.T) {
	clearPolicyEnv(t)

	v := reflect.ValueOf(loadRedisConfig())
	tp := v.Type()
	want := map[string]bool{"Addr": true, "Password": true, "DB": true, "Port": true}

	for i := range tp.NumField() {
		f := tp.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if !want[f.Name] {
			t.Errorf("RedisConfig.%s is a field nothing reads; it will look like a "+
				"setting and behave like a no-op", f.Name)
		}
	}
	if tp.NumField() != len(want) {
		t.Errorf("RedisConfig has %d fields, want exactly %d", tp.NumField(), len(want))
	}
}

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
