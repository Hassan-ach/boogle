package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file owns host-level state: whether we know anything about a host, and
// how we found out.
//
// The reason it is worth having is the reported failure. Before this existed,
// every URL on an unknown host caused a robots.txt fetch, and a host whose
// robots.txt could not be reached never recorded that fact -- the failure went
// into a request-scoped variable and was gone by the next URL. So twenty workers
// would each spend up to fifteen seconds retrying a dead domain, and none of them
// would ever conclude the domain was dead. The crawl did not loop on one URL; it
// looped on the *absence of a record*, and no repair at the loop level would have
// touched that.

// Host statuses, written to the host hash so a question like "is this host
// working" has an answer that survives its markers expiring.
const (
	// statusReady means robots.txt was read and the host is being crawled
	// normally.
	statusReady = "ready"
	// statusDegraded means the host answered, but with errors, and is being
	// crawled slowly.
	statusDegraded = "degraded"
)

// RobotsFetcher retrieves a robots.txt body and the HTTP status that came with it.
//
// It is a seam rather than an inline http.Get for two reasons. A test needs to
// exercise the caching, the negative caching and the parse without a network, and
// the single-shot transport Phase 4 moves out of utils.GetReq has to be able to
// sit underneath this call without the policy rules noticing. The status is
// returned because a 404 is not a failure -- it means the host published no
// rules, which is permission rather than an error, and the two are
// indistinguishable if the fetcher collapses them.
type RobotsFetcher func(ctx context.Context, url string) ([]byte, int, error)

// defaultRobotsTimeout bounds a robots.txt fetch.
//
// Without it, a host that accepts the connection and then never sends a body
// holds a crawl worker for as long as the OS TCP timeout allows -- thirty seconds
// or more, twenty times over, which is the loop this package was written to
// remove arriving by a different door.
const defaultRobotsTimeout = 10 * time.Second

// maxRobotsBytes caps how much of a robots.txt is read.
//
// A robots.txt is a few kilobytes in every file that has one. A host serving an
// unbounded one would otherwise be a way to make a crawler hold memory until it
// dies, so the read is bounded rather than trusted.
const maxRobotsBytes = 1 << 20

// WithRobotsFetcher returns a copy of the manager reading robots.txt through f.
//
// Production passes the spider's shared client so the crawl has one connection
// pool rather than two. A manager built without one gets a client of its own,
// which is fine for a test or a short-lived tool and worth replacing in a
// long-running process.
func (m *PolicyManager) WithRobotsFetcher(f RobotsFetcher) *PolicyManager {
	cp := *m
	if f != nil {
		cp.fetchRobots = f
	}
	return &cp
}

// newHTTPRobotsFetcher builds a fetcher over a shared client.
//
// A nil client gets one with a timeout of its own, because the alternative --
// http.DefaultClient, which has no timeout -- is exactly the hang described on
// defaultRobotsTimeout.
func newHTTPRobotsFetcher(client *http.Client, userAgent string) RobotsFetcher {
	if client == nil {
		client = &http.Client{Timeout: defaultRobotsTimeout}
	}

	return func(ctx context.Context, url string) ([]byte, int, error) {
		ctx, cancel := context.WithTimeout(ctx, defaultRobotsTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("build robots request: %w", err)
		}
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("fetch robots: %w", err)
		}
		defer drainAndClose(resp)

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBytes))
		if err != nil {
			return nil, resp.StatusCode, fmt.Errorf("read robots: %w", err)
		}
		return body, resp.StatusCode, nil
	}
}

// drainAndClose consumes the rest of a response before closing it, so the
// connection can be reused instead of being torn down.
//
// The drain is bounded for the same reason the read is: an unbounded drain of a
// body that never ends blocks the worker just as thoroughly as the read did.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRobotsBytes))
	_ = resp.Body.Close()
}

// EnsureHost resolves a host's policy state, fetching and parsing its robots.txt
// once and then trusting that copy for RobotsTTL.
//
// Three things happen here that did not happen before, and each is a fix:
//
//  1. The result is recorded, so the second URL on a host does not re-fetch.
//  2. A host whose robots.txt cannot be had is *marked*, so every subsequent URL
//     on it terminates at the marker check in O(1) instead of opening a
//     connection. The marker is the negative cache: it carries the TTL, and when
//     it expires the next EnsureHost is the implicit probe.
//  3. Crawl-delay is applied, floored, and stored in a field something reads.
//     It was parsed and written to a key nothing consulted, so the spider had no
//     rate limiting at all.
//
// A returned error is always "the host could not be resolved", never "this URL is
// bad". Admit turns it into a Defer, which is the right verdict: the URL may be
// perfectly crawlable once the host answers.
func (m *PolicyManager) EnsureHost(ctx context.Context, host string) (HostState, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return HostState{}, errEmptyHost
	}

	st, err := m.state.HostState(ctx, host)
	if err != nil {
		return HostState{}, fmt.Errorf("read host state for %s: %w", host, err)
	}

	// Known and fresh. This is the path ninety-nine percent of calls take, and
	// it is one hash read and no network.
	//
	// The test is the fetch timestamp rather than the name, because both state
	// backends return a HostState with Name set even for a host nobody has seen.
	// Keying on Name would treat every unknown host as known and never fetch
	// anything.
	if !st.RobotsFetchedAt.IsZero() && !m.robotsStale(st) {
		return st, nil
	}

	robotsURL, err := robotsURLFor(host)
	if err != nil {
		// A host we cannot build a robots.txt URL for is not a host we can crawl,
		// and no amount of retrying changes that. Marking it is the point: it
		// keeps the rest of the host's URLs from each making the same discovery.
		return HostState{}, m.markHostUnreachable(ctx, host, err)
	}

	body, status, err := m.fetchRobots(ctx, robotsURL)
	if err != nil {
		return HostState{}, m.markHostUnreachable(ctx, host, err)
	}

	// A 4xx other than 429 is not a failure. The convention every crawler
	// follows is that an unavailable robots.txt means no rules, not "this host is
	// broken" -- and treating a 404 as an error would make every host that
	// publishes no robots.txt look unreachable, which is most of the web.
	if status == http.StatusTooManyRequests || status >= 500 {
		reason := ReasonServerError
		if status == http.StatusTooManyRequests {
			reason = ReasonRateLimited
		}
		// A host refusing to be crawled is not a host that is gone. It gets a
		// cooldown, and the next URL on it is a Defer rather than a connection
		// attempt.
		if merr := m.state.SetMarker(ctx, host, MarkerCooldown, m.cfg.HostCooldown(1, 0)); merr != nil {
			m.log.Warn("could not set a cooldown on a refusing host",
				"host", host, "error", merr)
		}
		return HostState{}, &robotsRefusedError{host: host, status: status, reason: reason}
	}

	robots := parseRobots(string(body), m.cfg.UserAgent)

	// Crawl-delay is floored, never obeyed literally. A robots.txt asking for zero
	// -- or omitting the directive, which the old parser's hardcoded default of 5
	// masked -- must not mean "no delay", because that is how a crawler earns
	// being blocked.
	delay := time.Duration(robots.CrawlDelay) * time.Second
	if delay < m.cfg.MinCrawlDelay {
		delay = m.cfg.MinCrawlDelay
	}

	// The host record never raises the global page budget. A robots.txt is written
	// by whoever runs the site, and a directive that made the budget larger would
	// let a site opt itself into an unbounded crawl; only a reduction is a request
	// the crawler has reason to believe.
	maxPages := 0
	if robots.MaxPages > 0 && robots.MaxPages < m.cfg.MaxPagesPerHost {
		maxPages = robots.MaxPages
	}

	// Merged, not replaced. A robots re-read happens on a schedule measured in
	// hours and has no opinion about how many pages have been fetched or how many
	// times the host just refused; writing a whole new record would reset both.
	st = st.WithRobots(host, robots.Allow, robots.Disallow, robots.SiteMaps, delay, m.now())
	st.MaxPages = maxPages
	st.Status = statusReady
	if st.FirstSeen.IsZero() {
		st.FirstSeen = m.now()
	}

	if err := m.state.SaveHostState(ctx, host, st); err != nil {
		// The rules are in hand and the fetch will not be repeated for this URL,
		// so failing to cache them means the next URL re-fetches. That is worse
		// than the old behaviour but not incorrect, and returning the state
		// anyway means this URL is still crawlable -- failing closed here would
		// refuse a URL whose rules we demonstrably just read.
		m.log.Error("could not cache host state; the next url will re-fetch robots.txt",
			"host", host, "error", err)
	}
	return st, nil
}

// robotsRefusedError is a host that answered robots.txt with a status saying "not
// now".
//
// It is a distinct type rather than a formatted string because Admit has to tell
// this apart from a host that could not be reached, and the two demand different
// reasons in the stats hash: a site returning 503 is overloaded, while a site that
// never answers is gone. Matching on the message text would work until someone
// reworded it.
type robotsRefusedError struct {
	host   string
	status int
	reason Reason
}

func (e *robotsRefusedError) Error() string {
	return fmt.Sprintf("robots.txt for %s returned %d (%s)", e.host, e.status, e.reason)
}

// RobotsRefusedReason reports the reason a host's robots.txt was refused, and
// whether this error is such a refusal at all.
func RobotsRefusedReason(err error) (Reason, bool) {
	var e *robotsRefusedError
	if errors.As(err, &e) {
		return e.reason, true
	}
	return "", false
}

// robotsStale reports whether a host's cached robots.txt has to be re-read.
//
// The TTL is measured from when it was *fetched*, not from when it was last used,
// so a host crawled steadily has its rules re-read on schedule rather than never.
func (m *PolicyManager) robotsStale(st HostState) bool {
	if st.RobotsFetchedAt.IsZero() {
		return true
	}
	ttl := m.cfg.RobotsTTL
	if ttl <= 0 {
		// A zero TTL means "re-fetch on every URL", which is the bug this caching
		// exists to fix. It is read as "never re-read" instead, because a stale
		// rule that still says "Disallow: /private" errs towards politeness,
		// while a re-fetch storm errs towards load.
		m.log.Warn("robots ttl is not positive; cached rules will never be re-read",
			"host", st.Name, "robots_ttl", ttl)
		return false
	}
	return !m.now().Before(st.RobotsFetchedAt.Add(ttl))
}

// markHostUnreachable records that a host could not be reached. It is the negative
// cache, and it returns the cause so the caller can propagate it unchanged.
//
// It deliberately does *not* write a host state. Writing one with a fetch
// timestamp would be self-defeating: the marker expires after seconds or minutes
// while RobotsTTL is a day, so EnsureHost would find a "fresh" record for a host
// it never managed to read and never try again. That host would be permanently,
// silently uncrawlable -- a worse failure than the loop, because it looks like a
// site that simply has nothing to offer.
//
// So the marker's TTL is the whole of the negative cache, and it is the TTL
// Classify would have computed for a dead host, so both paths agree on when a host
// comes back.
func (m *PolicyManager) markHostUnreachable(ctx context.Context, host string, cause error) error {
	_, reason := categorizeError(cause)

	marker := MarkerDead
	ttl := m.cfg.DeadHostTTL(1)
	if isTimeout(cause) {
		// A timeout is the host being slow, not the host being gone. Marking it
		// dead here would abandon a site that was merely under load, and a slow
		// site is exactly the one worth crawling politely. This distinction is
		// the single most important one in the file: "did not answer" and "is not
		// there" look identical from the client and are opposite decisions.
		marker = MarkerCooldown
		ttl = m.cfg.HostCooldown(1, 0)
	}

	if err := m.state.SetMarker(ctx, host, marker, ttl); err != nil {
		m.log.Error("could not mark a host unreachable; the next url will retry it",
			"host", host, "reason", reason, "error", err)
	}

	m.log.Warn("host could not be resolved; its urls will be refused until the marker expires",
		"host", host, "marker", marker, "ttl", ttl, "reason", reason, "error", cause)
	return cause
}

// robotsURLFor builds the robots.txt URL for a host.
//
// Always https, always the bare host. The old code derived this from the URL that
// triggered it, so a page served over http asked for
// "http://example.com/robots.txt", got a redirect or a 404, and the result looked
// like an unreachable host.
func robotsURLFor(host string) (string, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return "", errEmptyHost
	}
	// A host carrying a port is reachable, and the port is kept, because dropping
	// it would silently crawl a different service. But a host with a path, a
	// space or a userinfo section is not a bare authority at all, and building a
	// URL from it would produce something that parses and does not resolve.
	if strings.ContainsAny(host, " \t\r\n/?#@") {
		return "", fmt.Errorf("host %q is not a bare hostname", host)
	}
	return "https://" + host + "/robots.txt", nil
}

// --- robots.txt parsing ---

// RobotsFile is a parsed robots.txt, reduced to the rules that apply to one
// crawler.
//
// It is not entity.Robots. That struct is the shape the spider passed around, and
// it carried a CrawlDelay of 5 baked in at construction, so "this host asked for
// no delay" and "this host asked for five seconds" were the same value and the
// first was unrepresentable.
type RobotsFile struct {
	Allow      []string
	Disallow   []string
	SiteMaps   []string
	CrawlDelay int

	// MaxPages is not a robots.txt directive. It exists so a host record can
	// carry a per-host page budget from whatever source eventually provides one,
	// and the value is floored against the global limit on the way in.
	MaxPages int
}

// robotsRules is one User-agent stanza's rules.
type robotsRules struct {
	allow    []string
	disallow []string
	delay    int
}

// parseRobots reads a robots.txt and reduces it to the rules applying to userAgent.
func parseRobots(txt, userAgent string) *RobotsFile {
	const wildcardAgent = "*"

	// rules is a map from an agent token to that group's rules. Order is kept
	// separately so the merged result is deterministic -- a map iterated directly
	// would produce a different Disallow slice on every call, and the slice is
	// written into a Redis hash and compared in tests.
	groups := map[string]*robotsRules{}
	var order []string
	group := func(agent string) *robotsRules {
		if g, ok := groups[agent]; ok {
			return g
		}
		g := &robotsRules{}
		groups[agent] = g
		order = append(order, agent)
		return g
	}

	var sitemaps []string
	// active is the set of groups the next rule line belongs to: the agents named
	// by the User-agent lines of the stanza in progress.
	var active []*robotsRules

	for _, raw := range strings.Split(txt, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A comment can follow a directive, and it has to come off before the
		// value is used, or "Disallow: /admin # the login page" is read as a path.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
			if line == "" {
				continue
			}
		}

		key, value, ok := splitDirective(line)
		if !ok {
			continue
		}

		switch key {
		case "user-agent":
			if value == "" {
				continue
			}
			// Consecutive User-agent lines are one stanza naming several agents,
			// and the rules below belong to all of them. Overwriting instead of
			// appending gave the rules to the last agent named, which is the shape
			// Google's own documentation recommends:
			//
			//	User-agent: *
			//	User-agent: googlebot
			//	Disallow: /private
			g := group(value)
			if !containsGroup(active, g) {
				active = append(active, g)
			}

		case "sitemap":
			// Sitemap belongs to no agent, so it is collected once wherever it
			// appears and not gated on the stanza.
			if value != "" {
				sitemaps = appendUnique(sitemaps, value)
			}

		case "disallow", "allow", "crawl-delay":
			// A rule before any User-agent line applies to nobody. The file is
			// malformed, and guessing either way means obeying or ignoring rules
			// nobody claimed.
			for _, g := range active {
				switch key {
				case "disallow":
					// "Disallow:" with no value is the standard's way of saying
					// nothing is disallowed. Recording it as a rule would record a
					// rule matching every path, which is how one blank line in a
					// real robots.txt once blocked an entire host.
					if value != "" {
						g.disallow = append(g.disallow, value)
					}
				case "allow":
					if value != "" {
						g.allow = append(g.allow, value)
					}
				case "crawl-delay":
					// A fractional delay ("Crawl-delay: 1.5") is not an integer
					// and is skipped rather than truncated to 1. Following it
					// literally would mean sleeping a third of a second between
					// requests, which is the opposite of what the site asked for.
					if n, err := strconv.Atoi(value); err == nil && n > 0 && n > g.delay {
						g.delay = n
					}
				}
			}

		default:
			// An unknown directive. Ignored, which is what the standard says to do
			// with one we do not implement.
		}
	}

	out := &RobotsFile{SiteMaps: sitemaps}

	// "*" always applies, and so does any token the crawler's own user agent
	// contains -- a site that names "BoogleBot" is addressing us directly, and one
	// that names only "googlebot" is not, which is why the second loop is a
	// substring test rather than an equality one.
	for _, agent := range order {
		if agent != wildcardAgent && !agentMatches(agent, userAgent) {
			continue
		}
		g := groups[agent]
		out.Allow = append(out.Allow, g.allow...)
		out.Disallow = append(out.Disallow, g.disallow...)
		if g.delay > out.CrawlDelay {
			out.CrawlDelay = g.delay
		}
	}

	// Repeated rules are not a correctness problem -- the matcher takes the most
	// specific -- but a file listing the same rule under forty agents otherwise
	// produces forty copies of every pattern in the host hash.
	out.Allow = dedupe(out.Allow)
	out.Disallow = dedupe(out.Disallow)
	out.SiteMaps = dedupe(out.SiteMaps)
	return out
}

// splitDirective splits "Key: value" into its parts, case-insensitively.
//
// The first colon is the separator, which is what makes a Sitemap value survive:
// "Sitemap: https://example.com/s.xml" splits into "sitemap" and the whole URL.
func splitDirective(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	if key == "" {
		return "", "", false
	}
	return key, strings.TrimSpace(line[i+1:]), true
}

// agentMatches reports whether a robots.txt agent token applies to our user
// agent.
//
// The standard's test is a case-insensitive substring match of the token in the
// crawler's user-agent string, which is why "BoogleBot" and "bot" both match a
// crawler identifying itself as "BoogleBot/1.0".
func agentMatches(token, userAgent string) bool {
	if token == "" || userAgent == "" {
		return false
	}
	return strings.Contains(strings.ToLower(userAgent), strings.ToLower(token))
}

func containsGroup(active []*robotsRules, g *robotsRules) bool {
	for _, a := range active {
		if a == g {
			return true
		}
	}
	return false
}

func appendUnique(in []string, v string) []string {
	for _, e := range in {
		if e == v {
			return in
		}
	}
	return append(in, v)
}

// dedupe removes repeats while keeping first-seen order, returning nil for an
// empty input so an unknown host stores no empty list.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
