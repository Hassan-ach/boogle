package policy

import (
	"context"
	"net/url"
)

// Sitemap discovery.
//
// A robots.txt is the only place a site advertises its sitemaps, so this is where
// they are read. It used to be read in the crawl loop, on the branch that
// handled "no metadata for this host yet" -- which is to say on the branch that
// every URL of a dead domain took, once per URL, holding a fetch slot each time.
// That branch is gone; the equivalent work happens once per robots.txt reading,
// here.
//
// The split of labour is the usual one: reading and parsing a sitemap is the
// parser's business, and it is a seam so this package does not have to know the
// XML. Deciding to do it at all, doing it exactly once, and putting what it finds
// in the frontier are this package's.

// SiteMapResolver expands a host's advertised sitemap URLs into page URLs.
//
// base is the host's root, which a resolver needs because a sitemap may be
// advertised as a bare path and a sitemap entry may itself be relative. It is
// passed rather than derived so that a resolver cannot resolve against a
// different host than the one whose robots.txt advertised the sitemap.
//
// A resolver that fails is not an error here. An unreadable sitemap costs the
// discovery of some URLs and nothing else, and treating it as a host failure would
// mark a perfectly healthy site dead over a file that is usually optional.
type SiteMapResolver func(ctx context.Context, base *url.URL, sitemaps []string) []string

// WithSiteMapResolver returns a copy of the manager expanding sitemaps through r.
//
// Without one, a host that advertises sitemaps is still crawled by link -- the
// sitemaps are simply not read. That is the right default: a manager used only to
// answer Admit has no business spending requests on discovery.
func (m *PolicyManager) WithSiteMapResolver(r SiteMapResolver) *PolicyManager {
	cp := *m
	if r != nil {
		cp.resolveSiteMaps = r
	}
	return &cp
}

// discoverSiteMaps queues what a host's sitemaps advertise, once.
//
// Called from EnsureHost, immediately after a robots.txt is read, because that is
// the one moment the advertised list is known to have changed. Calling it from the
// crawl loop instead would mean a hash read on every page to ask a question whose
// answer only differs once a day.
func (m *PolicyManager) discoverSiteMaps(ctx context.Context, st HostState) {
	if m.resolveSiteMaps == nil || len(st.SiteMaps) == 0 {
		return
	}

	// The claim is taken before the work, not after. Twenty workers reaching a
	// brand-new host at the same moment is the ordinary case, not the pathological
	// one, and sitemap entries are enqueued by inlink priority -- so a second pass
	// does not re-add the same URLs, it raises the score of URLs that have not
	// been crawled yet, every time the host's robots.txt is re-read, until they are
	// never crawled at all.
	// Claimed against the reading that advertised them, not against "now": a
	// worker that resolved robots.txt a while ago and lost the race to a slower one
	// that resolved it later must not queue the list a second time on the strength
	// of being the last to arrive.
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
		// Unreachable: st.Name is a hostname or came off a parsed URL. Claimed and
		// abandoned is still better than retrying over an unparseable host.
		m.log.Warn("could not build a base url for a host's sitemaps",
			"host", st.Name, "error", err)
		return
	}

	entries := m.resolveSiteMaps(ctx, base, st.SiteMaps)
	if len(entries) == 0 {
		return
	}

	// Filtered by the same rules Admit applies, but with the cheap half only and
	// no Redis. A sitemap routinely lists fifty thousand URLs, and running the
	// full gate over one would mean a hundred thousand round trips inside the
	// call that is trying to work out whether this host is crawlable at all. The
	// state-dependent rules -- visited, dead, cold, over budget -- are not skipped
	// so much as deferred: they are asked when the URL is popped, which is where
	// they have to be asked anyway. Nothing here decides that a URL is crawled.
	admitted, refused := m.filterLocally(st, entries)

	// Counted in aggregate rather than one HINCRBY per declined entry. Same
	// numbers an operator would get, one write instead of thousands, and the
	// per-host stats hash stays a summary of decisions rather than a log of file
	// parsing.
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
