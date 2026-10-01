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
func forceEnglishSubdomain(u *url.URL) {
	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")

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
		return
	}

	if languageSubtagRE.MatchString(potentialLang) {
		u.Host = "en." + rest
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

func IsDisallowed(path string, disallowed []string) bool {
	for _, d := range disallowed {
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, `.^$*+?[]|()`) {
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

func isDisallowed(p string, disallowed []string) bool {
	return IsDisallowed(p, disallowed)
}
