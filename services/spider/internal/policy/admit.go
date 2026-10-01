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

// Admit is the single decision point for a discovered URL. It answers whether the
// URL may be fetched now and, when not, why. The gates run cheapest-first --
// shape, then duplicate, then host -- so the common refusals cost no Redis round
// trip beyond the visited check.
//
// Every refusal is counted against its host, which is what the stats hash is for.
func (m *PolicyManager) Admit(ctx context.Context, rawURL string) (*Verdict, error) {
	if scheme, ok := schemeOf(rawURL); !ok {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	} else if scheme == "" {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	} else if scheme != "http" && scheme != "https" {
		return m.refuse(ctx, "", rawURL, Skip, ReasonNonHTTPScheme, time.Time{})
	}

	norm, ok := utils.CanonicalizeUrl(rawURL, "")
	if !ok {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	}

	parsed, err := url.Parse(norm)
	if err != nil || parsed.Hostname() == "" {
		return m.refuse(ctx, "", rawURL, Skip, ReasonMalformedURL, time.Time{})
	}

	host := hostKey(parsed)

	visited, err := m.state.IsVisited(ctx, norm)
	if err != nil {
		return m.unavailable(ctx, host, norm, err)
	}
	if visited {
		return m.refuse(ctx, host, norm, Skip, ReasonVisited, time.Time{})
	}

	hostState, verdict, err := m.hostGate(ctx, host)
	if err != nil {
		return nil, err
	}
	if verdict != nil {
		return m.refuse(ctx, host, norm, verdict.Kind, verdict.Reason, verdict.Until)
	}

	if hostState.RobotsFetchedAt.IsZero() || m.robotsStale(hostState) {
		resolved, rerr := m.EnsureHost(ctx, host)
		if rerr != nil {
			return m.refuse(ctx, host, norm, Defer, hostResolutionReason(rerr),
				m.now().Add(m.cfg.URLBackoff(1)))
		}
		hostState = resolved
	}

	if m.rollWindow(ctx, host, &hostState) {
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

	if reason, ok := m.admitsLocally(hostState, parsed); !ok {
		return m.refuse(ctx, host, norm, Skip, reason, time.Time{})
	}

	return &Verdict{
		Kind:   Allow,
		Reason: ReasonOK,
		Host:   hostState.ToEntity(m.cfg.URLMaxAttempts),
	}, nil
}

func (m *PolicyManager) admitsLocally(st HostState, parsed *url.URL) (Reason, bool) {
	if allowed, rule := NewRobotsRules(st.Allow, st.Disallow).Allows(parsed.EscapedPath()); !allowed {
		m.log.Debug("refused by robots.txt", "url", parsed.String(), "rule", rule)
		return ReasonRobotsDisallow, false
	}

	rules := m.rules()

	if rules.SkipsPath(parsed.Path) {
		return ReasonPathDisallowed, false
	}

	if rules.SkipsExtension(parsed.Path) {
		return ReasonExtensionSkipped, false
	}

	if rules.SkipsWikiLanguageSubpage(parsed.Path) {
		return ReasonLanguageNotEnglish, false
	}
	return ReasonOK, true
}

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

var errWindowReset = errors.New("could not reset an expired crawl window")

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
	st.PagesCrawled = 0
	st.WindowStartedAt = m.now()
	m.log.Info("host page window elapsed; the budget has been reset",
		"host", host, "window", m.cfg.HostColdPeriod, "max_pages", m.cfg.MaxPagesPerHost)
	return false
}

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

func hostResolutionReason(err error) Reason {
	if reason, ok := RobotsRefusedReason(err); ok {
		return reason
	}
	return ReasonHostMetadataUnknown
}

func (m *PolicyManager) AdmitLinks(ctx context.Context, links []string) (admitted []string, refused map[string]Reason) {
	admitted = make([]string, 0, len(links))
	refused = make(map[string]Reason)

	for _, link := range links {
		verdict, err := m.Admit(ctx, link)
		if err != nil {
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

func canonicalOf(link string) string {
	if norm, ok := utils.CanonicalizeUrl(link, ""); ok {
		return norm
	}
	return link
}

func FormatVerdict(v *Verdict) string {
	if v == nil {
		return "none"
	}
	if v.Until.IsZero() {
		return fmt.Sprintf("%s/%s", v.Kind, v.Reason)
	}
	return fmt.Sprintf("%s/%s until %s", v.Kind, v.Reason, v.Until.UTC().Format(time.RFC3339))
}
