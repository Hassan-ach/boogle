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

var (
	// Pagination and sort parameters generate unbounded near-duplicate URLs for
	// one page of content, so they are dropped during canonicalization instead of
	// being crawled. The content parameter (id, p, article) is deliberately absent.
	disallowQueryParams = []string{
		"sort", "page", "filter", "q", "search",
	}

	langSubdomainDomains = []string{
		"wikipedia.org",
		"wikibooks.org",
		"wikivoyage.org",
	}

	languageSubtagRE = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8}){0,2}$`)
)

func NormalizeUrl(raw string, baseHost string) (string, bool) {
	return CanonicalizeUrl(raw, baseHost)
}

// CanonicalizeUrl reduces a URL to the single form the spider treats as "the
// same page", which is what makes the visited set and the frontier deduplicate
// work. Two links are the same page if they differ only in scheme, www, case,
// default port, fragment, trailing slash or query-parameter order.
//
// The rewrite is aggressive on purpose: a crawler that treats /a and /a/ as
// distinct pages fetches both and indexes duplicate content.
func CanonicalizeUrl(raw string, baseHost string) (string, bool) {
	if !utf8.ValidString(raw) || strings.HasPrefix(raw, "#") {
		return "", false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	hadTrailingSlash := strings.HasSuffix(u.Path, "/")
	u.Path = path.Clean("/" + u.Path)
	if u.Path == "/" {
		u.Path = ""
	}

	normalizeURLParts(u, baseHost, hadTrailingSlash)
	return u.String(), true
}

func isValidQueryParam(q string) bool {
	return !slices.Contains(disallowQueryParams, q)
}

func normalizeURLParts(u *url.URL, baseHost string, hadTrailingSlash bool) {
	u.Scheme = "https"

	if u.Host == "" && baseHost != "" {
		u.Host = baseHost
	}

	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")

	forceEnglishSubdomain(u)

	u.Fragment = ""

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

	if hadTrailingSlash && u.Path != "" && !strings.HasSuffix(u.Path, "/") {
		if len(u.Query()) > 0 || strings.Contains(u.Path, ".") {
			u.Path += "/"
		}
	}
}

// forceEnglishSubdomain rewrites fr.wikipedia.org to en.wikipedia.org, since
// the index stores English only and the two are otherwise separate pages with
// identical content. It is scoped to the Wikimedia hosts in langSubdomainDomains
// rather than applied generally, because elsewhere a language subdomain is a
// genuinely different site.
//
// The caller has already lowercased and stripped one leading www. The second
// pass here is deliberate: it is what turns www.www.example.com into
// example.com, which a single strip leaves as www.example.com.
func forceEnglishSubdomain(u *url.URL) {
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")

	for _, d := range langSubdomainDomains {
		if u.Host == d || strings.HasSuffix(u.Host, "."+d) {
			foldLangSubdomain(u)
			return
		}
	}
}

// foldLangSubdomain rewrites the leading label to "en" when it looks like a
// language subtag.
//
// The label-boundary check in the caller is what keeps fr.notwikipedia.org out.
// A bare strings.HasSuffix against the domain matches any host ending in those
// characters, and that host belongs to someone else: folding it would merge two
// unrelated sites into one host bucket in the visited set and the frontier.
func foldLangSubdomain(u *url.URL) {
	hostParts := strings.Split(u.Host, ".")
	if len(hostParts) < 3 {
		return
	}

	potentialLang := hostParts[0]
	if potentialLang == "en" {
		return
	}

	if languageSubtagRE.MatchString(potentialLang) {
		u.Host = "en." + strings.Join(hostParts[1:], ".")
	}
}

func NormalizeUrls(raws []string, baseHost string) []string {
	return CanonicalizeUrls(raws, baseHost)
}

func CanonicalizeUrls(raws []string, baseHost string) []string {
	result := make([]string, 0, len(raws))
	for _, raw := range raws {
		if norm, ok := CanonicalizeUrl(raw, baseHost); ok {
			result = append(result, norm)
		}
	}
	return result
}
