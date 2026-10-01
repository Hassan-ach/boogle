package policy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

// Admit decides whether a candidate URL may be fetched now.
//
// This is the pre-fetch gate. It is the only place in the tree that may answer
// "should this be crawled", and the only function that returns Allow. Everything
// downstream -- the frontier, the crawl loop, the fetch pool -- takes its answer
// from here.
//
// # The ordering is the design
//
// The checks run cheapest-and-most-decisive first, and that is not a style
// preference. The expensive ones -- a regex over the path, a lookup in a rule
// table -- are last, so a URL that is going to be refused by a Redis marker never
// pays for them. The reason the crawl used to loop forever is entirely explained
// by this table: a dead host's robots.txt was re-fetched for every URL it owned,
// before any of the cheap facts about that host had been read.
//
// Order, with the cost of each step:
//
//	1  canonicalise        pure
//	2  scheme is http(s)   pure
//	3  already visited     one Redis read
//	4  host markers        one Redis read  (dead -> cold -> cooldown)
//	5  host state          one hash read
//	6  robots known?       network, only for a host we have never resolved
//	7  page budget         arithmetic on what step 5 returned
//	8  robots Allow/Disallow  one regex per rule, on a path already canonicalised
//	9  path prefixes       one pass over the table
//	10 file extension      one pass over the table
//	11 wiki language subpage  one regex
//
// Steps 3 to 7 are pure key-existence checks and cost no network. That is the
// whole fix: after a host's first connection failure, every subsequent URL on it
// terminates at step 4 in O(1).
//
// # Every failure is a reason
//
// A refusal names one of a bounded set of reasons and counts it against the host.
// The precise text -- which rule matched, which extension, what the robots.txt
// said -- does not fit in a counter field and is not what an operator aggregates
// over; it goes in the log and in the URL's state hash. What the stats hash gives
// is the question worth asking: of everything this host offered, how much did we
// decline, and why.
//
// # Nothing here fetches a page
//
// Admit reads Redis and, for a host never seen before, its robots.txt. It never
// touches the page, so it cannot make the expensive decision -- what to do about a
// body -- before the cheap ones have been made. Classify owns that.
func (m *PolicyManager) Admit(ctx context.Context, rawURL string) (*Verdict, error) {
	// Step 1. The scheme, read off the raw string.
	//
	// This has to happen before canonicalisation, because canonicalisation is what
	// rewrites the scheme to https -- by which point "mailto:someone@example.com"
	// has become a host-less https URL and there is nothing left to say it was
	// never a page. Reading it first is also the only way to keep a mailbox out of
	// the host tables: without it, that link has an "@" in its path and looks like
	// a broken reference to a domain built out of someone's address.
	//
	// A link carrying a javascript: or data: payload is not a document and never
	// becomes one, however it is spelled.
	if scheme, ok := schemeOf(rawURL); !ok {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	} else if scheme == "" {
		// No scheme at all. A relative reference -- "/a/b", "#section" -- has no
		// meaning until it is resolved against a document, and Admit has no
		// document to resolve it against, so it is a malformed candidate rather
		// than a URL with the wrong scheme.
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	} else if scheme != "http" && scheme != "https" {
		return m.refuse(ctx, "", rawURL, Skip, ReasonNonHTTPScheme, time.Time{})
	}

	// Step 2. Canonicalise, which also rejects the strings that are not URLs at
	// all: invalid UTF-8, or a bare fragment. Doing it here means every later step
	// runs on one stable spelling of the URL, and it is the same spelling the
	// frontier will store, so a URL that is admitted and a URL that is
	// rediscovered agree on their visited key.
	//
	// CanonicalizeUrl, not NormalizeUrl. The latter also applies the skip rules,
	// and running them here would put the decisions in two places with two sets of
	// tables -- the arrangement this file exists to end.
	norm, ok := utils.CanonicalizeUrl(rawURL, "")
	if !ok {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	}

	parsed, err := url.Parse(norm)
	if err != nil || parsed.Hostname() == "" {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	}

	// hostKey, not Hostname: the record has to be findable by the robots.txt URL
	// built from it later, and a non-default port is part of which site this is.
	host := hostKey(parsed)

	// Step 3. Already visited.
	//
	// This is the cheapest question and the most common answer, and it is worth
	// its own round trip: without it, every rediscovered URL re-runs eight checks
	// to be told what a set membership already said.
	visited, err := m.state.IsVisited(ctx, norm)
	if err != nil {
		return m.unavailable(ctx, host, norm, err)
	}
	if visited {
		return m.refuse(ctx, host, norm, Skip, ReasonVisited, time.Time{})
	}

	// Steps 4 to 5. Markers, then the host hash.
	//
	// hostGate reads the markers before the hash, which inverts the order in the
	// table above and is the better arrangement: a dead host is answered from the
	// marker alone, without a hash read that would only confirm what the marker
	// already said. The plan puts the hash first; doing it the other way costs one
	// extra round trip on every URL of a dead host, which is precisely the case
	// this work exists to make cheap.
	hostState, verdict, err := m.hostGate(ctx, host)
	if err != nil {
		return nil, err
	}
	if verdict != nil {
		// The reason is already set by hostGate; count it here so the refusal has
		// the same observability as one produced below.
		return m.refuse(ctx, host, norm, verdict.Kind, verdict.Reason, verdict.Until)
	}

	// Step 6. Has this host ever been resolved, and are its rules still current?
	//
	// The staleness test has to be here as well as inside EnsureHost. Testing only
	// for "never fetched" would make a host's robots.txt permanent: the rules would
	// be read once and never again, so a site that tightened its rules after we
	// first visited would be crawled in full regardless. The check costs nothing,
	// because the timestamp was read in step 5.
	if hostState.RobotsFetchedAt.IsZero() || m.robotsStale(hostState) {
		resolved, rerr := m.EnsureHost(ctx, host)
		if rerr != nil {
			// Defer, not Skip. The host may be perfectly crawlable in ten seconds,
			// and a terminal verdict here would strand every URL ever discovered on
			// it -- including the ones that were perfectly good.
			//
			// The due time is one backoff interval, for the reason on
			// PolicyManager.unavailable: the URL is already off the frontier by the
			// time this verdict is produced, so a Defer with no due time does not
			// mean "ask again soon", it means "this URL is lost". An unresolvable
			// host is the ordinary case where that bites -- a domain that stopped
			// resolving takes every URL anyone ever linked to with it, and drops the
			// whole set rather than parking it.
			//
			// The reason distinguishes "we could not ask" from "the host said no".
			// They are counted separately because the first is our problem and the
			// second is the site's, and an operator reading a full stats hash needs
			// to tell them apart.
			return m.refuse(ctx, host, norm, Defer, hostResolutionReason(rerr),
				m.now().Add(m.cfg.URLBackoff(1)))
		}
		hostState = resolved
	}

	// Step 7. The page budget, and the window it belongs to.
	//
	// The rollover is here rather than in the state layer because deciding a window
	// has ended is a policy question, and because the in-memory and Redis backends
	// implement the same arithmetic differently. Doing it once, here, means there
	// is one answer rather than two.
	//
	// The window is HostColdPeriod long, the same figure that decides how long an
	// exhausted host stays cold, because the window *is* the cold period: a host
	// that spends its budget sleeps for one window and comes back with a full
	// one. Deriving the rollover from window_started_at rather than from the cold
	// marker's expiry means the counter still resets correctly if the marker was
	// evicted or the spider was down for the whole cold period.
	if m.rollWindow(ctx, host, &hostState) {
		// ResetWindow failed, so the count on hand is not trustworthy. Reading it
		// anyway could refuse a host that has actually finished its window; and
		// ignoring it could fetch past a real budget. Neither is knowable, so the
		// honest answer is to stop and try again.
		return m.unavailable(ctx, host, norm, errWindowReset)
	}

	if hostState.BudgetExhausted(m.cfg.MaxPagesPerHost) {
		until := m.now().Add(m.cfg.HostColdPeriod)
		if err := m.state.SetMarker(ctx, host, MarkerCold, m.cfg.HostColdPeriod); err != nil {
			m.log.Warn("could not mark a budget-exhausted host cold; it will be re-checked next url",
				"host", host, "error", err)
		}
		return m.refuse(ctx, host, norm, Skip, ReasonHostBudgetExhausted, until)
	}

	// Steps 8 to 11. The rules. All in-memory, all cheap, and all last, so a URL
	// refused above never pays for them.
	if reason, ok := m.admitsLocally(hostState, parsed); !ok {
		return m.refuse(ctx, host, norm, Skip, reason, time.Time{})
	}

	return &Verdict{
		Kind:   Allow,
		Reason: ReasonOK,
		Host:   hostState.ToEntity(m.cfg.URLMaxAttempts),
	}, nil
}

// admitsLocally runs the rules that need no stored state, returning the reason a
// URL is refused or (ReasonOK, true) if it survives.
//
// It is the tail of Admit, factored out because a sitemap is filtered by exactly
// these rules and cannot afford the state-dependent half. One function rather than
// two is the point: a second copy of "is this path crawlable" is how "/report.pdf/"
// came to be refused by one caller and fetched by another, and no code review
// catches that when both copies look correct.
//
// The order is deliberate. robots.txt is the site's own instruction to us and gets
// first refusal; our tables are our judgement about URLs in general, and applying
// them first would discard a page the site explicitly invited us to.
func (m *PolicyManager) admitsLocally(st HostState, parsed *url.URL) (Reason, bool) {
	if allowed, rule := NewRobotsRules(st.Allow, st.Disallow).Allows(parsed.EscapedPath()); !allowed {
		m.log.Debug("refused by robots.txt", "url", parsed.String(), "rule", rule)
		return ReasonRobotsDisallow, false
	}

	rules := m.rules()

	// Path prefixes, compared on whole segments. "/cart" does not swallow
	// "/cartoon".
	if rules.SkipsPath(parsed.Path) {
		return ReasonPathDisallowed, false
	}

	// File extensions.
	if rules.SkipsExtension(parsed.Path) {
		return ReasonExtensionSkipped, false
	}

	// Translated copies of pages that were not worth indexing anyway.
	if rules.SkipsWikiLanguageSubpage(parsed.Path) {
		return ReasonLanguageNotEnglish, false
	}
	return ReasonOK, true
}

// filterLocally applies admitsLocally to a batch of URLs belonging to a known
// host, splitting them into the ones worth queueing and the reasons the rest were
// not.
//
// A URL that will not canonicalise, or that parses without a host, is refused as
// malformed -- the same answer Admit gives it. That matters here because a
// sitemap is a machine-generated file that has been known to contain relative
// entries and typos, and a queue is the wrong place to find out.
func (m *PolicyManager) filterLocally(st HostState, urls []string) (admitted []string, refused []Reason) {
	admitted = make([]string, 0, len(urls))
	refused = make([]Reason, 0, len(urls))

	for _, raw := range urls {
		norm, ok := utils.CanonicalizeUrl(raw, "")
		if !ok {
			refused = append(refused, ReasonMalformedURL)
			continue
		}
		parsed, err := url.Parse(norm)
		if err != nil || parsed.Hostname() == "" {
			refused = append(refused, ReasonMalformedURL)
			continue
		}
		reason, ok := m.admitsLocally(st, parsed)
		if !ok {
			refused = append(refused, reason)
			continue
		}
		admitted = append(admitted, norm)
	}
	return admitted, refused
}

// errWindowReset marks a state read whose count could not be trusted because the
// window could not be rolled over.
var errWindowReset = errors.New("could not reset an expired crawl window")

// rollWindow starts a new page-budget window for a host whose previous one has
// elapsed, and reports whether it tried.
//
// A host that has never been resolved has no window and nothing to roll, so it is
// left alone; its window begins when EnsureHost first records it.
//
// The non-positive-period case is treated as "no rollover". A zero cold period
// with a non-zero budget would otherwise reset the counter on every URL, making
// the budget unenforceable and re-deriving the loop this package exists to stop
// in a new form -- a host could then be crawled without limit by anyone who set
// HOST_COLD_PERIOD_SEC=0. Reading it as "the window never ends" leaves a fixed
// budget, which is the conservative direction: it can crawl too little, not too
// much.
func (m *PolicyManager) rollWindow(ctx context.Context, host string, st *HostState) bool {
	if st.WindowStartedAt.IsZero() || m.cfg.HostColdPeriod <= 0 {
		return false
	}
	if m.now().Before(st.WindowStartedAt.Add(m.cfg.HostColdPeriod)) {
		return false
	}
	if err := m.state.ResetWindow(ctx, host, m.now()); err != nil {
		m.log.Error("could not start a new page window for a host whose window expired",
			"host", host, "window_started_at", st.WindowStartedAt, "error", err)
		return true
	}
	// The local copy has to be updated too. It was read before the reset, so it
	// still carries the expired window's count, and the budget check immediately
	// below would refuse the host on a number that has already been cleared.
	st.PagesCrawled = 0
	st.WindowStartedAt = m.now()
	m.log.Info("host page window elapsed; the budget has been reset",
		"host", host, "window", m.cfg.HostColdPeriod, "max_pages", m.cfg.MaxPagesPerHost)
	return false
}

// schemeOf reads the scheme off a raw URL string without touching anything else
// about it.
//
// It reports false only for a string that will not parse. A string with no scheme
// at all yields the empty scheme, which is not http or https and so is refused by
// the caller's comparison -- a relative reference such as "/a/b" has no meaning
// until it is resolved against something, and Admit has nothing to resolve it
// against.
func schemeOf(raw string) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return strings.ToLower(u.Scheme), true
}

// hostResolutionReason maps a failure to resolve a host onto the reason it is
// counted as.
//
// The distinction is between a host that refused and a host that did not answer,
// because EnsureHost has already recorded the difference as a cooldown or a dead
// marker. This only decides what the counter says; the marker is what the crawl
// actually obeys.
//
// The typed check matters here. A string match on the message would classify a
// refusal correctly until someone reworded the message, and then every overloaded
// host would start being counted as an unreachable one -- in a way nothing would
// notice, because both are refusals and both are correct as decisions.
func hostResolutionReason(err error) Reason {
	if reason, ok := RobotsRefusedReason(err); ok {
		return reason
	}
	return ReasonHostMetadataUnknown
}

// AdmitLinks filters a batch of candidate links, returning the ones that may be
// fetched and the reason each refusal happened.
//
// It exists so a discovered link can be counted. The old filtering was a loop over
// ValidateLinks that dropped bad URLs silently: a page yielding forty links,
// eleven of them PDFs, produced eleven entries in the frontier that were later
// removed by a different function with no record of why, so the crawl's logs said
// nothing about the pages it declined.
//
// Links are canonicalised and filtered here but *not* enqueued. Enqueueing is the
// caller's decision, because a caller batching a page's links usually wants to
// enqueue them as one operation and marking them visited here would fight that.
func (m *PolicyManager) AdmitLinks(ctx context.Context, links []string) (admitted []string, refused map[string]Reason) {
	admitted = make([]string, 0, len(links))
	refused = make(map[string]Reason)

	for _, link := range links {
		verdict, err := m.Admit(ctx, link)
		if err != nil {
			// Admit's own error path is a state failure, and Admit has already
			// turned it into a closed verdict. An error reaching here is a bug in
			// Admit rather than a condition to recover from, so the link is refused
			// and the failure is logged. Refusing is the only safe direction: an
			// error here that let the link through would fetch a URL the policy
			// layer never approved.
			m.log.Error("admit returned an error; refusing the link", "link", link, "error", err)
			refused[link] = ReasonPolicyUnavailable
			continue
		}
		switch verdict.Kind {
		case Allow:
			admitted = append(admitted, canonicalOf(link))
		default:
			refused[link] = verdict.Reason
		}
	}
	return admitted, refused
}

// canonicalOf recovers the canonical spelling of a link Admit approved.
//
// Admit canonicalises internally and reports its verdict, but a Verdict
// deliberately does not carry the normalised URL: it is a decision, not a
// rewritten string, and adding one would invite a caller to mistake "this URL is
// allowed" for "use this URL". A caller that wants the canonical form gets it
// from Discover, which stores it.
//
// Falling back to the original link is deliberate: it is better to enqueue a
// slightly denormalised URL than to drop an approved one, because a non-canonical
// key is a duplicate-fetch risk and a missing page is a hole in the index.
func canonicalOf(link string) string {
	if norm, ok := utils.CanonicalizeUrl(link, ""); ok {
		return norm
	}
	return link
}

// FormatVerdict renders a verdict for a log line. It exists so the crawl log
// carries the reason and the until together, which is the pair an operator needs:
// a refusal with a deadline is a wait, and one without is a decision.
func FormatVerdict(v *Verdict) string {
	if v == nil {
		return "none"
	}
	if v.Until.IsZero() {
		return fmt.Sprintf("%s/%s", v.Kind, v.Reason)
	}
	return fmt.Sprintf("%s/%s until %s", v.Kind, v.Reason, v.Until.UTC().Format(time.RFC3339))
}
