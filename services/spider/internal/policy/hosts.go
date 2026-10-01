package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	statusReady    = "ready"
	statusDegraded = "degraded"
)

type RobotsFetcher func(ctx context.Context, url string) ([]byte, int, error)

var defaultRobotsTimeout = 10 * time.Second

// Robots bodies are read through a LimitReader at this size, so a host serving
// an enormous or endless robots.txt cannot exhaust the spider's memory.
const maxRobotsBytes = 1 << 20

func (m *PolicyManager) WithRobotsFetcher(f RobotsFetcher) *PolicyManager {
	cp := *m
	if f != nil {
		cp.fetchRobots = f
	}
	return &cp
}

func (m *PolicyManager) WithRobotsClient(client *http.Client) *PolicyManager {
	cp := *m
	if client != nil {
		cp.fetchRobots = newHTTPRobotsFetcher(client, defaultUserAgent(cp.cfg))
	}
	return &cp
}

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

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRobotsBytes))
	_ = resp.Body.Close()
}

func (m *PolicyManager) EnsureHost(ctx context.Context, host string) (HostState, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return HostState{}, errEmptyHost
	}

	st, err := m.state.HostState(ctx, host)
	if err != nil {
		return HostState{}, fmt.Errorf("read host state for %s: %w", host, err)
	}

	if !st.RobotsFetchedAt.IsZero() && !m.robotsStale(st) {
		return st, nil
	}

	robotsURL, err := robotsURLFor(host)
	if err != nil {
		return HostState{}, m.markHostUnreachable(ctx, host, err)
	}

	body, status, err := m.fetchRobots(ctx, robotsURL)
	if err != nil {
		return HostState{}, m.markHostUnreachable(ctx, host, err)
	}

	if status == http.StatusTooManyRequests || status >= 500 {
		reason := ReasonServerError
		if status == http.StatusTooManyRequests {
			reason = ReasonRateLimited
		}
		if merr := m.state.SetMarker(ctx, host, MarkerCooldown, m.cfg.HostCooldown(1, 0)); merr != nil {
			m.log.Warn("could not set a cooldown on a refusing host",
				"host", host, "error", merr)
		}
		return HostState{}, &robotsRefusedError{host: host, status: status, reason: reason}
	}

	robots := parseRobots(string(body), m.cfg.UserAgent)

	delay := time.Duration(robots.CrawlDelay) * time.Second
	if delay < m.cfg.MinCrawlDelay {
		delay = m.cfg.MinCrawlDelay
	}

	maxPages := 0
	if robots.MaxPages > 0 && robots.MaxPages < m.cfg.MaxPagesPerHost {
		maxPages = robots.MaxPages
	}

	st = st.WithRobots(host, robots.Allow, robots.Disallow, robots.SiteMaps, delay, m.now())
	st.MaxPages = maxPages
	if st.Status == "" {
		st.Status = statusReady
	}
	if st.FirstSeen.IsZero() {
		st.FirstSeen = m.now()
	}

	if err := m.state.SaveHostState(ctx, host, st); err != nil {
		m.log.Error("could not cache host state; the next url will re-fetch robots.txt",
			"host", host, "error", err)
	}

	m.discoverSiteMaps(ctx, st)

	return st, nil
}

type robotsRefusedError struct {
	host   string
	status int
	reason Reason
}

func (e *robotsRefusedError) Error() string {
	return fmt.Sprintf("robots.txt for %s returned %d (%s)", e.host, e.status, e.reason)
}

func RobotsRefusedReason(err error) (Reason, bool) {
	var e *robotsRefusedError
	if errors.As(err, &e) {
		return e.reason, true
	}
	return "", false
}

func (m *PolicyManager) robotsStale(st HostState) bool {
	if st.RobotsFetchedAt.IsZero() {
		return true
	}
	ttl := m.cfg.RobotsTTL
	if ttl <= 0 {
		m.log.Warn("robots ttl is not positive; cached rules will never be re-read",
			"host", st.Name, "robots_ttl", ttl)
		return false
	}
	return !m.now().Before(st.RobotsFetchedAt.Add(ttl))
}

func (m *PolicyManager) markHostUnreachable(ctx context.Context, host string, cause error) error {
	_, reason := categorizeError(cause)

	marker := MarkerDead
	ttl := m.cfg.DeadHostTTL(1)
	if isTimeout(cause) {
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

func robotsURLFor(host string) (string, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return "", errEmptyHost
	}
	if strings.ContainsAny(host, " \t\r\n/?#@") {
		return "", fmt.Errorf("host %q is not a bare hostname", host)
	}
	return "https://" + host + "/robots.txt", nil
}

type RobotsFile struct {
	Allow      []string
	Disallow   []string
	SiteMaps   []string
	CrawlDelay int

	MaxPages int
}

type robotsRules struct {
	allow    []string
	disallow []string
	delay    int
}

// parseRobots reads robots.txt into the rules that apply to userAgent.
//
// active is the set of groups a bare Allow/Disallow/Crawl-delay line feeds, which
// is how the format scopes rules to the user-agent lines above them. At the end
// every group whose name matches userAgent, plus "*", is merged by union and the
// largest crawl-delay wins. Merging all matches is deliberately more permissive
// than the spec's "pick the most specific group": a site that splits rules across
// two of our agent names should end up restricted, not ignored.
func parseRobots(txt, userAgent string) *RobotsFile {
	const wildcardAgent = "*"

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
	var active []*robotsRules

	for _, raw := range strings.Split(txt, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
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
			g := group(value)
			if !containsGroup(active, g) {
				active = append(active, g)
			}

		case "sitemap":
			if value != "" {
				sitemaps = appendUnique(sitemaps, value)
			}

		case "disallow", "allow", "crawl-delay":
			for _, g := range active {
				switch key {
				case "disallow":
					if value != "" {
						g.disallow = append(g.disallow, value)
					}
				case "allow":
					if value != "" {
						g.allow = append(g.allow, value)
					}
				case "crawl-delay":
					if n, err := strconv.Atoi(value); err == nil && n > 0 && n > g.delay {
						g.delay = n
					}
				}
			}

		default:
		}
	}

	out := &RobotsFile{SiteMaps: sitemaps}

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

	out.Allow = dedupe(out.Allow)
	out.Disallow = dedupe(out.Disallow)
	out.SiteMaps = dedupe(out.SiteMaps)
	return out
}

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

type SiteMapResolver func(ctx context.Context, base *url.URL, sitemaps []string) []string

func (m *PolicyManager) WithSiteMapResolver(r SiteMapResolver) *PolicyManager {
	cp := *m
	if r != nil {
		cp.resolveSiteMaps = r
	}
	return &cp
}

// discoverSiteMaps resolves and enqueues a host's sitemaps. ClaimSiteMaps makes
// this safe to call on every EnsureHost: the claim is keyed on the robots fetch
// time, so only the spider that fetched robots first does the work. The cost of
// a lost race is that sitemaps may be queued twice, which the frontier tolerates;
// the cost of skipping the claim would be never resolving them at all.
func (m *PolicyManager) discoverSiteMaps(ctx context.Context, st HostState) {
	if m.resolveSiteMaps == nil || len(st.SiteMaps) == 0 {
		return
	}

	won, err := m.state.ClaimSiteMaps(ctx, st.Name, st.RobotsFetchedAt)
	if err != nil {
		m.log.Warn("could not claim a host's sitemaps; they may be queued twice",
			"host", st.Name, "error", err)
		return
	}
	if !won {
		return
	}

	base, err := url.Parse("https://" + st.Name)
	if err != nil {
		m.log.Warn("could not build a base url for a host's sitemaps",
			"host", st.Name, "error", err)
		return
	}

	entries := m.resolveSiteMaps(ctx, base, st.SiteMaps)
	if len(entries) == 0 {
		return
	}

	admitted, refused := m.filterLocally(st, entries)

	byReason := make(map[Reason]int, len(refused))
	for _, r := range refused {
		byReason[r]++
	}
	for reason, n := range byReason {
		if err := m.state.CountReason(ctx, st.Name, reason); err != nil {
			m.log.Warn("could not record a sitemap refusal",
				"host", st.Name, "reason", reason, "error", err)
		}
		m.log.Info("sitemap entries declined",
			"host", st.Name, "reason", reason, "count", n)
	}

	m.Discover(ctx, admitted)
}
