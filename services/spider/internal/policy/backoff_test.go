package policy

import (
	"math"
	"testing"
	"time"
)

func TestDeadHostTTL(t *testing.T) {
	c := DefaultBackoffConfig()

	want := map[int]time.Duration{
		0:  60 * time.Second,
		1:  120 * time.Second,
		2:  240 * time.Second,
		3:  480 * time.Second,
		4:  960 * time.Second,
		5:  1920 * time.Second,
		6:  3840 * time.Second,
		7:  3840 * time.Second,
		20: 3840 * time.Second,
	}
	for attempts, expect := range want {
		if got := c.DeadHostTTL(attempts); got != expect {
			t.Errorf("DeadHostTTL(%d) = %v, want %v", attempts, got, expect)
		}
	}

	if got := c.DeadHostTTL(-5); got != 60*time.Second {
		t.Errorf("DeadHostTTL(-5) = %v, want %v", got, 60*time.Second)
	}
}

func TestHostCooldown(t *testing.T) {
	c := DefaultBackoffConfig()

	tests := []struct {
		name       string
		attempts   int
		crawlDelay time.Duration
		want       time.Duration
	}{
		{"first failure uses the base", 0, 0, 10 * time.Second},
		{"second failure doubles", 1, 0, 20 * time.Second},
		{"third failure doubles again", 2, 0, 40 * time.Second},
		{"fourth failure doubles again", 3, 0, 80 * time.Second},
		{"fifth is capped at exponent 3", 4, 0, 80 * time.Second},

		{"crawl-delay raises the floor", 0, 30 * time.Second, 30 * time.Second},
		{"crawl-delay raises the floor and still doubles", 1, 30 * time.Second, 60 * time.Second},
		{"a long crawl-delay still doubles from itself", 2, 30 * time.Second, 120 * time.Second},

		{"a large crawl-delay does not outrun the backoff", 3, 30 * time.Second, 240 * time.Second},
		{"a short crawl-delay does not lower the backoff", 3, time.Second, 80 * time.Second},

		{"a crawl-delay below the base does not lower it", 0, 5 * time.Second, 10 * time.Second},
		{"a crawl-delay of zero does not lower it", 0, 0, 10 * time.Second},
		{"a negative crawl-delay does not lower it", 0, -time.Hour, 10 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.HostCooldown(tc.attempts, tc.crawlDelay); got != tc.want {
				t.Errorf("HostCooldown(%d, %v) = %v, want %v", tc.attempts, tc.crawlDelay, got, tc.want)
			}
		})
	}
}

func TestURLBackoff(t *testing.T) {
	c := DefaultBackoffConfig()

	want := map[int]time.Duration{
		0:  30 * time.Second,
		1:  60 * time.Second,
		2:  120 * time.Second,
		3:  240 * time.Second,
		4:  480 * time.Second,
		5:  900 * time.Second,
		50: 900 * time.Second,
	}
	for attempt, expect := range want {
		if got := c.URLBackoff(attempt); got != expect {
			t.Errorf("URLBackoff(%d) = %v, want %v", attempt, got, expect)
		}
	}

	if got := c.URLBackoff(-3); got != 30*time.Second {
		t.Errorf("URLBackoff(-3) = %v, want %v", got, 30*time.Second)
	}
}

func TestSchedulesAreMonotonic(t *testing.T) {
	c := DefaultBackoffConfig()

	type schedule struct {
		name      string
		at        func(int) time.Duration
		growBelow int
	}
	schedules := []schedule{
		{"URLBackoff", func(n int) time.Duration { return c.URLBackoff(n) }, 5},
		{"DeadHostTTL", c.DeadHostTTL, c.DeadHostMaxExp},
		{"HostCooldown", func(n int) time.Duration { return c.HostCooldown(n, 0) }, c.HostCooldownMaxExp},
	}

	for _, s := range schedules {
		t.Run(s.name, func(t *testing.T) {
			for i := 0; i < 100; i++ {
				if got, prev := s.at(i+1), s.at(i); got < prev {
					t.Errorf("attempt %d: %v went backwards from %v", i, got, prev)
				}
			}
			for i := 0; i < s.growBelow; i++ {
				if got, prev := s.at(i+1), s.at(i); got <= prev {
					t.Errorf("attempt %d: %v did not grow past %v below the clamp at %d",
						i, got, prev, s.growBelow)
				}
			}
		})
	}
}

func TestSchedulesAreTotal(t *testing.T) {
	bases := []time.Duration{0, -time.Second, time.Nanosecond, time.Second, time.Hour, math.MaxInt64 / 4}
	attempts := []int{-100, -1, 0, 1, 30, 61, 62, 63, 64, 1000}

	for _, base := range bases {
		for _, n := range attempts {
			for _, maxExp := range []int{-1, 0, 1, 6, 62} {
				if d := scale(base, n, maxExp); d <= 0 {
					t.Errorf("scale(%v, %d, %d) = %v, must be positive", base, n, maxExp, d)
				}
			}
		}
	}
}

func TestScaleDoesNotOverflow(t *testing.T) {
	huge := time.Duration(math.MaxInt64 / 2)

	for _, n := range []int{0, 1, 10, 40, 61, 62, 63, 100} {
		d := scale(huge, n, -1)
		if d <= 0 {
			t.Errorf("scale(MaxInt64/2, %d) = %v, overflowed to non-positive", n, d)
		}
	}

	if got := scale(0, 0, 6); got != time.Second {
		t.Errorf("scale(0, 0, 6) = %v, want %v", got, time.Second)
	}
}

func TestDefaultConfigIsUsable(t *testing.T) {
	c := DefaultConfig()

	if c.MaxPagesPerHost != 5000 {
		t.Errorf("MaxPagesPerHost = %d, want 5000", c.MaxPagesPerHost)
	}
	if c.HostColdPeriod != time.Hour {
		t.Errorf("HostColdPeriod = %v, want 1h", c.HostColdPeriod)
	}
	if c.MinCrawlDelay != time.Second {
		t.Errorf("MinCrawlDelay = %v, want 1s -- robots.txt omitting Crawl-delay must not mean no delay", c.MinCrawlDelay)
	}
	if c.MaxRedirects != 10 {
		t.Errorf("MaxRedirects = %d, want 10", c.MaxRedirects)
	}
	if c.MaxBodyBytes != 10<<20 {
		t.Errorf("MaxBodyBytes = %d, want %d", c.MaxBodyBytes, 10<<20)
	}
	if c.FrontierPopBatch != 16 || c.DelayedPromoteBatch != 100 {
		t.Errorf("batch sizes = %d/%d, want 16/100", c.FrontierPopBatch, c.DelayedPromoteBatch)
	}
	if c.URLStateTTL <= 0 || c.RobotsTTL <= 0 {
		t.Errorf("TTLs must be positive so state expires on its own: url=%v robots=%v", c.URLStateTTL, c.RobotsTTL)
	}
	if c.DeadHostBase <= 0 || c.HostCooldownBase <= 0 || c.URLBackoffBase <= 0 || c.URLBackoffMax <= 0 {
		t.Error("every backoff base and ceiling must be positive")
	}
}

func TestDeadProbeFloorExceedsDeadBase(t *testing.T) {
	c := DefaultBackoffConfig()

	if c.DeadProbeAfter < c.DeadHostBase {
		t.Errorf("DeadProbeAfter %v is below DeadHostBase %v: a url would return while "+
			"its host is still marked dead, get skipped as terminal, and the domain "+
			"would never be probed again", c.DeadProbeAfter, c.DeadHostBase)
	}
}
