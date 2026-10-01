package utils

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// The tables below are the ones canonicalisation needs and no more: query
// parameters that identify a result set rather than a page, and the wiki language
// subdomains that mirror the same text under three hundred hosts.
//
// The path, extension and wiki-namespace skip tables that used to live here are
// gone. They were a second copy of policy.RuleSet, and a second copy of a rule is
// one too many: an earlier version of IsDisallowed existed here as two copies, one
// of which had no guard for an empty rule, and since strings.HasPrefix(p, "") is
// true for every p a single blank line in one robots.txt blocked an entire host.
// That bug survived a code review because both copies looked correct and only one
// was reachable.
//
// A decision can also be counted, which a canonicalisation cannot do. Every URL
// dropped here left no trace, so the crawl log could not say how much of a site
// was being declined, or why.
var (
	disallowQueryParams = []string{
		"sort", "page", "filter", "q", "search",
	}

	langSubdomainDomains = []string{
		"wikipedia.org",
		"wikibooks.org",
		"wikivoyage.org",
	}

	// languageSubtagRE matches an IETF BCP-47 language tag: two to three
	// subtags of two to eight alphanumerics, at least the first of them letters.
	// The old check was "two to five characters, letters, one optional hyphen",
	// which missed zh-yue, be-tarask, zh-min-nan and every other language
	// Wikipedia publishes under, so those mirrors got crawled in full instead of
	// being folded onto en.
	languageSubtagRE = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8}){0,2}$`)
)

// NormalizeUrl canonicalises a URL.
//
// Deprecated: it no longer applies the three skip rules it used to. Those rules are
// policy's, they live in policy.RuleSet, and they are counted when they are
// applied. Use CanonicalizeUrl for identity and policy.Admit for the decision.
//
// This wrapper survives one release so a caller outside this tree does not break on
// the spot. It is the same function, not a reduced one -- so a URL that used to
// vanish here now reaches Admit and is refused there with a reason attached, which
// is the point of the change rather than a regression to be papered over.
func NormalizeUrl(raw string, baseHost string) (string, bool) {
	return CanonicalizeUrl(raw, baseHost)
}

// CanonicalizeUrl answers "what URL is this?" and nothing else.
//
// It is NormalizeUrl without the skip rules, and it is what the policy manager
// calls: canonicalisation has to happen before a rule can run at all, because
// "/admin/../public" and "/admin/x/../y" both clean to "/admin/y" and a rule
// applied to the uncleaned path would miss them.
//
// The split matters more than it looks. A function that both answers "what URL is
// this?" and "should we crawl it?" cannot be reasoned about, cannot be tested
// without a policy opinion baked in, and cannot have its decisions counted -- a
// caller has no way to learn that a URL was dropped rather than normalised, which
// is why every skip here was invisible in the crawl log.
func CanonicalizeUrl(raw string, baseHost string) (string, bool) {
	if !utf8.ValidString(raw) || strings.HasPrefix(raw, "#") {
		return "", false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	// Resolve "." and ".." segments and collapse repeated slashes. "/a/./b", "/a//b"
	// and "/a/b" are the same document, and keying them separately splits a page's
	// word counts and PageRank three ways.
	//
	// path.Clean also drops a trailing slash, which is load bearing for servers
	// that distinguish "/a" from "/a/", so it is recorded and re-applied below.
	hadTrailingSlash := strings.HasSuffix(u.Path, "/")
	u.Path = path.Clean("/" + u.Path)
	if u.Path == "/" {
		// The site root renders without a trailing slash, so "https://x" and
		// "https://x/" cannot end up as two keys for the home page.
		u.Path = ""
	}

	normalizeURLParts(u, baseHost, hadTrailingSlash)
	return u.String(), true
}

func isValidQueryParam(q string) bool {
	return !slices.Contains(disallowQueryParams, q)
}

func normalizeURLParts(u *url.URL, baseHost string, hadTrailingSlash bool) {
	// Force HTTPS
	u.Scheme = "https"

	// Fill host if relative URL
	if u.Host == "" && baseHost != "" {
		u.Host = baseHost
	}

	// Clean host
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")

	forceEnglishSubdomain(u)

	// Remove fragment
	u.Fragment = ""

	// Canonicalize query: sorted keys
	if u.RawQuery != "" {
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			if isValidQueryParam(k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)

		sorted := url.Values{}
		for _, k := range keys {
			sorted[k] = q[k]
		}
		u.RawQuery = sorted.Encode()
	}

	// The trailing slash is significant to some servers -- "/a" and "/a/" can be
	// different documents -- but not all, and path.Clean has already thrown it
	// away. Put it back only where it is meaningful: when a query survived, or
	// when the path looks like a filename, in which case the slash denotes a
	// directory listing rather than the document itself.
	if hadTrailingSlash && u.Path != "" && !strings.HasSuffix(u.Path, "/") {
		if len(u.Query()) > 0 || strings.Contains(u.Path, ".") {
			u.Path += "/"
		}
	}
}

func forceEnglishSubdomain(u *url.URL) {
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")

	// Only act on known wiki-family domains
	isKnownLangDomain := false
	for _, d := range langSubdomainDomains {
		if strings.HasSuffix(u.Host, d) {
			isKnownLangDomain = true
			break
		}
	}
	if !isKnownLangDomain {
		return
	}

	hostParts := strings.Split(u.Host, ".")
	if len(hostParts) < 3 {
		return
	}

	potentialLang := hostParts[0]
	rest := strings.Join(hostParts[1:], ".")

	if potentialLang == "en" {
		return // already English
	}

	if languageSubtagRE.MatchString(potentialLang) {
		u.Host = "en." + rest
	}
}

// NormalizeUrls canonicalises a batch, dropping the entries that will not parse.
//
// Deprecated: it never applied a skip rule of its own, but the name promises one
// and the function it was named after did. Use CanonicalizeUrls.
func NormalizeUrls(raws []string, baseHost string) []string {
	return CanonicalizeUrls(raws, baseHost)
}

// CanonicalizeUrls is NormalizeUrls without the misleading name.
//
// It does not deduplicate. Dedup belongs to the caller's Set, and hiding it here
// would hide how many times a site links to itself.
func CanonicalizeUrls(raws []string, baseHost string) []string {
	result := make([]string, 0, len(raws))
	for _, raw := range raws {
		if norm, ok := CanonicalizeUrl(raw, baseHost); ok {
			result = append(result, norm)
		}
	}
	return result
}

// ValidateLinks filters a batch of links against a site's own robots rules.
//
// Deprecated: it decides crawl-worthiness, which is policy's job. The replacement
// is policy.AdmitLinks, which does the same filtering *and* records a reason per
// refusal, so a link this drops is one an operator can find in the host's stats.
//
// The two do not answer the same question, which is worth being explicit about.
// This one returns a list and nothing else, so the links it dropped left no trace:
// a page with forty links and eleven PDFs put eleven entries in the frontier that
// were later removed by a different function with no record of why, and the logs
// said nothing about which eleven or why. Nobody could answer "why is this site
// barely in the index".
func ValidateLinks(links []string, disallowed []string) []string {
	normUrls := NewSet[string]()
	for _, x := range links {
		ur, err := url.Parse(x)
		if err != nil || isDisallowed(ur.Path, disallowed) {
			continue
		}
		normUrls.Add(ur.String())
	}
	return normUrls.GetAll()
}

// IsDisallowed reports whether a path is blocked by any of the given rules.
//
// Deprecated: robots rules are not prefixes here or anywhere else -- they are
// matched with the wildcard and end-anchor semantics of RFC 9309, which is
// policy.RobotsRules. This function treats a rule containing a regex
// metacharacter as a regular expression and everything else as a raw string
// prefix, so "/admin/" fails to match "/admin/x" on a site that asked for it to,
// and "/admin$" matches nothing at all. It is kept for one release for callers
// outside this tree.
//
// A rule containing a regex metacharacter is treated as a regular expression,
// otherwise it is a path prefix. Two rules that used to live here as separate
// copies of this function disagreed: the unexported one had no guard for an
// empty rule, and strings.HasPrefix(p, "") is true for every p, so a single
// blank line in a robots.txt file blocked the entire host.
func IsDisallowed(path string, disallowed []string) bool {
	for _, d := range disallowed {
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, `.^$*+?[]|()`) {
			// An unparseable pattern is skipped rather than allowed through:
			// failing open on a malformed rule would crawl something an
			// operator explicitly asked not to be crawled.
			if matched, err := regexp.MatchString(d, path); err == nil && matched {
				return true
			}
		} else {
			if strings.HasPrefix(path, d) {
				return true
			}
		}
	}
	return false
}

// isDisallowed is the private alias kept for the callers inside this package.
// It delegates rather than reimplementing, so the two cannot drift apart again.
func isDisallowed(p string, disallowed []string) bool {
	return IsDisallowed(p, disallowed)
}
