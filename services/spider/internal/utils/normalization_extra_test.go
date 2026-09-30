package utils

import (
	"net/url"
	"testing"
)

// ── NormalizeUrl: identity ───────────────────────────────────────────────────

// TestNormalizeUrlIsIdempotent is the property the whole index depends on.
//
// The spider normalises a URL on the way into the frontier and the engine
// normalises the stored URL again when scoring. If normalisation were not
// idempotent, the same page would index under two different keys and split its
// PageRank and word counts in half.
func TestNormalizeUrlIsIdempotent(t *testing.T) {
	raws := []string{
		"http://www.Example.COM/page",
		"https://example.com/page/",
		"http://example.com//page",
		"http://example.com/page?b=2&a=1",
		"http://example.com/page?page=3&keep=1",
		"http://example.com/path#frag",
		"/relative/path",
		"http://fr.wikipedia.org/wiki/Go",
		"http://example.com",
		"http://example.com/a/b/c/",
	}

	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			once, ok := NormalizeUrl(raw, "example.com")
			if !ok {
				t.Skipf("NormalizeUrl(%q) rejected the URL", raw)
			}

			twice, ok := NormalizeUrl(once, "example.com")
			if !ok {
				t.Fatalf("NormalizeUrl rejected its own output %q", once)
			}
			if once != twice {
				t.Errorf("not idempotent:\n  once  = %q\n  twice = %q", once, twice)
			}
		})
	}
}

func TestNormalizeUrlForcesHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/a",
		"https://example.com/a",
		"//example.com/a",
		"/a",
	} {
		t.Run(raw, func(t *testing.T) {
			got, ok := NormalizeUrl(raw, "example.com")
			if !ok {
				t.Skip("rejected")
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("NormalizeUrl produced unparseable %q: %v", got, err)
			}
			if u.Scheme != "https" {
				t.Errorf("scheme = %q, want https", u.Scheme)
			}
		})
	}
}

func TestNormalizeUrlStripsWww(t *testing.T) {
	// A www and non-www host are the same site; indexing both halves the
	// PageRank of every page on it.
	for _, raw := range []string{
		"http://www.example.com/a",
		"http://WWW.EXAMPLE.COM/a",
		"http://www.www.example.com/a",
	} {
		t.Run(raw, func(t *testing.T) {
			got, ok := NormalizeUrl(raw, "")
			if !ok {
				t.Fatalf("NormalizeUrl(%q) rejected the URL", raw)
			}
			if got != "https://example.com/a" {
				t.Errorf("NormalizeUrl(%q) = %q, want https://example.com/a", raw, got)
			}
		})
	}
}

func TestNormalizeUrlKeepsNonWwwSubdomains(t *testing.T) {
	// Only "www." is stripped. "www2" or a real subdomain like "blog" is part
	// of the host's identity.
	for _, tc := range []struct{ raw, want string }{
		{"http://blog.example.com/a", "https://blog.example.com/a"},
		{"http://www2.example.com/a", "https://www2.example.com/a"},
		{"http://wwwx.example.com/a", "https://wwwx.example.com/a"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := NormalizeUrl(tc.raw, "")
			if !ok {
				t.Fatalf("NormalizeUrl(%q) rejected the URL", tc.raw)
			}
			if got != tc.want {
				t.Errorf("NormalizeUrl(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeUrlFillsInTheBaseHostForRelativeURLs(t *testing.T) {
	for _, tc := range []struct{ raw, base, want string }{
		{"/a", "example.com", "https://example.com/a"},
		{"a", "example.com", "https://example.com/a"},
		{"./a", "example.com", "https://example.com/a"},
		{"../a", "example.com", "https://example.com/a"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := NormalizeUrl(tc.raw, tc.base)
			if !ok {
				t.Fatalf("NormalizeUrl(%q) rejected the URL", tc.raw)
			}
			if got != tc.want {
				t.Errorf("NormalizeUrl(%q, %q) = %q, want %q", tc.raw, tc.base, got, tc.want)
			}
		})
	}
}

func TestNormalizeUrlDoesNotOverwriteAnAbsoluteHost(t *testing.T) {
	// A link to another host must stay on that host, not get pulled onto the
	// base host.
	got, ok := NormalizeUrl("https://other.com/page", "example.com")
	if !ok {
		t.Fatal("rejected an absolute cross-host URL")
	}
	if got != "https://other.com/page" {
		t.Errorf("NormalizeUrl() = %q, want the original host preserved", got)
	}
}

func TestNormalizeUrlStripsFragments(t *testing.T) {
	// #section links are the same document and would otherwise be indexed
	// separately.
	for _, raw := range []string{
		"http://example.com/page#one",
		"http://example.com/page#two",
		"http://example.com/page#",
	} {
		t.Run(raw, func(t *testing.T) {
			got, ok := NormalizeUrl(raw, "")
			if !ok {
				t.Fatal("rejected")
			}
			if got != "https://example.com/page" {
				t.Errorf("NormalizeUrl(%q) = %q, want the fragment stripped", raw, got)
			}
		})
	}
}

func TestNormalizeUrlRejectsBareFragments(t *testing.T) {
	for _, raw := range []string{"#top", "#", "#!/route"} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeUrl(raw, "example.com"); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection", raw, got)
			}
		})
	}
}

func TestNormalizeUrlRejectsInvalidUTF8(t *testing.T) {
	// A malformed byte sequence can never round-trip through the database or
	// the URL, so it has to be rejected at the edge.
	if got, ok := NormalizeUrl("http://example.com/\xff\xfe", ""); ok {
		t.Errorf("NormalizeUrl accepted invalid UTF-8, got %q", got)
	}
}

func TestNormalizeUrlRejectsAControlCharacterInTheHost(t *testing.T) {
	for _, raw := range []string{
		"http://exa\nmple.com/a",
		"http://example.com/a\tb",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeUrl(raw, ""); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection of a control character", raw, got)
			}
		})
	}
}

func TestNormalizeUrlSortsQueryParameters(t *testing.T) {
	// Two orderings of the same query are the same page.
	a, ok := NormalizeUrl("http://example.com/p?b=2&a=1", "")
	if !ok {
		t.Fatal("rejected a")
	}
	b, ok := NormalizeUrl("http://example.com/p?a=1&b=2", "")
	if !ok {
		t.Fatal("rejected b")
	}
	if a != b {
		t.Errorf("query order leaked into the key: %q vs %q", a, b)
	}
	if a != "https://example.com/p?a=1&b=2" {
		t.Errorf("NormalizeUrl() = %q, want sorted parameters", a)
	}
}

func TestNormalizeUrlDropsPaginationAndSearchParams(t *testing.T) {
	// /p?page=1 through /p?page=50000 are one page, and a site search result
	// set is an infinite crawl.
	got, ok := NormalizeUrl("http://example.com/p?page=3&keep=1", "")
	if !ok {
		t.Fatal("rejected")
	}
	if got != "https://example.com/p?keep=1" {
		t.Errorf("NormalizeUrl() = %q, want the pagination param dropped", got)
	}
}

func TestNormalizeUrlDropsAQueryThatIsEntirelyExcluded(t *testing.T) {
	got, ok := NormalizeUrl("http://example.com/p?page=3", "")
	if !ok {
		t.Fatal("rejected")
	}
	if got != "https://example.com/p" {
		t.Errorf("NormalizeUrl() = %q, want https://example.com/p", got)
	}
}

func TestNormalizeUrlKeepsThePathWhenAQuerySurvives(t *testing.T) {
	// Trailing-slash removal is gated on an empty query, because /a/?b=1 and
	// /a?b=1 are different documents to a server that treats the slash as
	// significant.
	got, ok := NormalizeUrl("http://example.com/a/?b=1", "")
	if !ok {
		t.Fatal("rejected")
	}
	if got != "https://example.com/a/?b=1" {
		t.Errorf("NormalizeUrl() = %q, want the trailing slash kept alongside a query", got)
	}
}

func TestNormalizeUrlRemovesTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://example.com/a/", "https://example.com/a"},
		{"http://example.com/a/b/", "https://example.com/a/b"},
		{"http://example.com/", "https://example.com"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := NormalizeUrl(tc.raw, "")
			if !ok {
				t.Fatalf("NormalizeUrl(%q) rejected the URL", tc.raw)
			}
			if got != tc.want {
				t.Errorf("NormalizeUrl(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeUrlKeepsATrailingSlashWhenThePathLooksLikeAFile(t *testing.T) {
	got, ok := NormalizeUrl("http://example.com/a/index.html/", "")
	if !ok {
		t.Fatal("rejected")
	}
	if got != "https://example.com/a/index.html/" {
		t.Errorf("NormalizeUrl() = %q, want the trailing slash kept", got)
	}
}

func TestNormalizeUrlSkipsDisallowedPathPrefixes(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/login",
		"http://example.com/admin/users",
		"http://example.com/cart",
		"http://example.com/search",
		"http://example.com/settings/profile",
		"http://example.com/404",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeUrl(raw, ""); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection", raw, got)
			}
		})
	}
}

func TestNormalizeUrlKeepsOrdinaryPathsThatMerelyStartLikeADisallowedOne(t *testing.T) {
	// A prefix match must not eat a legitimate path that happens to share a
	// few leading characters. "cartoon" is a real word and "searching" is a
	// real page; both start with a disallowed prefix.
	for _, raw := range []string{
		"http://example.com/cartoon",
		"http://example.com/searching",
		"http://example.com/logins-archive",
		"http://example.com/settings-overview",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, ok := NormalizeUrl(raw, ""); !ok {
				t.Errorf("NormalizeUrl(%q) was rejected by prefix matching", raw)
			}
		})
	}
}

func TestNormalizeUrlSkipsBinaryAndMediaExtensions(t *testing.T) {
	for _, ext := range []string{
		".pdf", ".doc", ".docx", ".zip", ".tar", ".gz", ".exe", ".dmg", ".apk",
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg",
		".mp3", ".wav", ".flac", ".mp4", ".mkv", ".webm",
		".css", ".js", ".ico",
	} {
		t.Run(ext, func(t *testing.T) {
			if got, ok := NormalizeUrl("http://example.com/file"+ext, ""); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection", ext, got)
			}
		})
	}
}

func TestNormalizeUrlSkipsExtensionsCaseInsensitively(t *testing.T) {
	// Servers serve /file.PDF and /file.pdf the same way.
	if got, ok := NormalizeUrl("http://example.com/FILE.PDF", ""); ok {
		t.Errorf("NormalizeUrl() = %q, want rejection for an uppercase extension", got)
	}
}

// A download endpoint is not necessarily a binary. The extension check runs on
// the path, so ?file=a.pdf survives normalisation on purpose: the path has no
// extension, and guessing at query values would drop real HTML pages whose URLs
// happen to carry a "file" parameter. Documented rather than asserted either way.
func TestNormalizeUrlDoesNotGuessAtQueryStringContents(t *testing.T) {
	got, ok := NormalizeUrl("http://example.com/download?file=a.pdf", "")
	if !ok {
		t.Fatal("NormalizeUrl() rejected a page whose path has no binary extension")
	}
	if got != "https://example.com/download?file=a.pdf" {
		t.Errorf("NormalizeUrl() = %q, want the URL kept as-is", got)
	}
}

func TestNormalizeUrlForcesEnglishOnWikiSubdomains(t *testing.T) {
	// The index is English, and one page mirrored across 300 language
	// subdomains would take 300 slots for the same text.
	for _, tc := range []struct{ raw, want string }{
		{"http://fr.wikipedia.org/wiki/Go", "https://en.wikipedia.org/wiki/Go"},
		{"http://de.wikipedia.org/wiki/Go", "https://en.wikipedia.org/wiki/Go"},
		{"http://pt.wikipedia.org/wiki/Go", "https://en.wikipedia.org/wiki/Go"},
		{"http://zh-yue.wikipedia.org/wiki/Go", "https://en.wikipedia.org/wiki/Go"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := NormalizeUrl(tc.raw, "")
			if !ok {
				t.Fatalf("NormalizeUrl(%q) rejected the URL", tc.raw)
			}
			if got != tc.want {
				t.Errorf("NormalizeUrl(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeUrlLeavesEnglishWikiAlone(t *testing.T) {
	got, ok := NormalizeUrl("http://en.wikipedia.org/wiki/Go", "")
	if !ok {
		t.Fatal("rejected")
	}
	if got != "https://en.wikipedia.org/wiki/Go" {
		t.Errorf("NormalizeUrl() = %q, want the host unchanged", got)
	}
}

func TestNormalizeUrlLeavesNonWikiHostsAlone(t *testing.T) {
	// "docs.example.com" is a real subdomain, not a language code, and
	// rewriting it to "en.example.com" would send the crawl to a dead host.
	for _, tc := range []struct{ raw, want string }{
		{"http://docs.example.com/a", "https://docs.example.com/a"},
		{"http://blog.example.com/a", "https://blog.example.com/a"},
		{"http://en.example.com/a", "https://en.example.com/a"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := NormalizeUrl(tc.raw, "")
			if !ok {
				t.Fatalf("rejected %q", tc.raw)
			}
			if got != tc.want {
				t.Errorf("NormalizeUrl(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeUrlSkipsWikiNamespacePages(t *testing.T) {
	// /Template:Foo/de and /Help:Bar/en are documentation about a page, not
	// the page itself, and there are tens of thousands of them.
	for _, raw := range []string{
		"http://en.wikipedia.org/wiki/Template:Foo/de",
		"http://en.wikipedia.org/wiki/Help:Bar/en",
		"http://en.wikipedia.org/wiki/Manual:Baz/en",
		"http://en.wikipedia.org/wiki/Extension:Qux/de",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeUrl(raw, ""); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection", raw, got)
			}
		})
	}
}

func TestNormalizeUrlRejectsAMalformedURL(t *testing.T) {
	for _, raw := range []string{
		"://missing-scheme",
		"http://[::1]:namedport/",
		"ht tp://example.com/a",
		"http://exa mple.com/",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeUrl(raw, ""); ok {
				t.Errorf("NormalizeUrl(%q) = %q, want rejection", raw, got)
			}
		})
	}
}

// ── NormalizeUrls ────────────────────────────────────────────────────────────

func TestNormalizeUrlsDropsTheRejectedEntries(t *testing.T) {
	got := NormalizeUrls([]string{
		"http://example.com/a",
		"http://example.com/login",
		"http://example.com/b",
		"#fragment",
		"http://example.com/c.pdf",
	}, "")

	want := []string{"https://example.com/a", "https://example.com/b"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeUrls() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("NormalizeUrls()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeUrlsPreservesInputOrder(t *testing.T) {
	// The result drives the crawl frontier, so reordering it changes which
	// pages get crawled before the per-host budget runs out.
	got := NormalizeUrls([]string{
		"http://example.com/zebra",
		"http://example.com/apple",
		"http://example.com/mango",
	}, "")

	want := []string{
		"https://example.com/zebra",
		"https://example.com/apple",
		"https://example.com/mango",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeUrls() = %v, want the input order %v", got, want)
		}
	}
}

func TestNormalizeUrlsDoesNotDeduplicate(t *testing.T) {
	// Dedup is a separate concern handled by the caller's Set. Silently
	// deduping here would hide how many times a site links to itself.
	got := NormalizeUrls([]string{
		"http://example.com/a",
		"http://www.example.com/a/",
		"http://example.com/a#frag",
	}, "")

	if len(got) != 3 {
		t.Errorf("NormalizeUrls() = %v, want three entries; dedup is not this function's job", got)
	}
}

func TestNormalizeUrlsOnEmptyInput(t *testing.T) {
	got := NormalizeUrls(nil, "example.com")
	if len(got) != 0 {
		t.Errorf("NormalizeUrls(nil) = %v, want an empty slice", got)
	}
}

func TestNormalizeUrlsOnAllRejected(t *testing.T) {
	got := NormalizeUrls([]string{"#a", "http://example.com/login", "#b"}, "")
	if len(got) != 0 {
		t.Errorf("NormalizeUrls() = %v, want an empty slice", got)
	}
}

// ── IsDisallowed / isDisallowed ─────────────────────────────────────────────

func TestIsDisallowedWithNoRulesAllowsEverything(t *testing.T) {
	for _, path := range []string{"/", "/admin", "/anything"} {
		if IsDisallowed(path, nil) {
			t.Errorf("IsDisallowed(%q, nil) = true, want false", path)
		}
		if IsDisallowed(path, []string{}) {
			t.Errorf("IsDisallowed(%q, []) = true, want false", path)
		}
	}
}

// TestIsDisallowedIgnoresEmptyRules is the regression test.
//
// An empty rule used to be taken as a prefix match, and strings.HasPrefix(path,
// "") is true for every path, so a single stray "" in the robots.txt rules
// silently blocked the entire host. A blank line in a robots.txt file produces
// exactly that.
func TestIsDisallowedIgnoresEmptyRules(t *testing.T) {
	rules := []string{"", "/admin/", ""}

	for _, path := range []string{"/", "/public", "/docs/guide", "/index.html"} {
		t.Run(path, func(t *testing.T) {
			if IsDisallowed(path, rules) {
				t.Errorf("IsDisallowed(%q, %v) = true; an empty rule must not block the whole host", path, rules)
			}
		})
	}
}

func TestIsDisallowedStillAppliesRealRulesAlongsideEmptyOnes(t *testing.T) {
	rules := []string{"", "/admin/", ""}

	if !IsDisallowed("/admin/users", rules) {
		t.Error("a real rule alongside empty ones was ignored")
	}
	if IsDisallowed("/administrative-guide", rules) {
		t.Error("a path merely starting with a rule's characters was blocked")
	}
	// The rule is "/admin/", so the bare "/admin" is not covered by it. The
	// exported helper is a raw prefix match, unlike NormalizeUrl's segment
	// matcher; robots.txt rules are copied verbatim, so that is deliberate.
	if IsDisallowed("/admin", rules) {
		t.Error("the rule /admin/ should not match the bare path /admin")
	}
}

func TestIsDisallowedTreatsRuleLikePatternsAsRegexes(t *testing.T) {
	rules := []string{`^/secret/.*`, `/admin/`, `\.php$`}

	// The argument is a path, never a path plus query: every caller passes
	// url.URL.Path, which excludes the query string.
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/secret/keys", true},
		{"/secret/", true},
		{"/admin/users", true},
		{"/index.php", true},
		{"/public/index.html", false},
		{"/", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := IsDisallowed(tc.path, rules); got != tc.want {
				t.Errorf("IsDisallowed(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestIsDisallowedIgnoresAnInvalidRegex(t *testing.T) {
	// A malformed pattern must not take the host down; skipping it is the same
	// choice ValidateLinks already makes.
	rules := []string{"[unclosed", "/admin/"}

	if !IsDisallowed("/admin/x", rules) {
		t.Error("a valid rule alongside an invalid one was ignored")
	}
	if got := IsDisallowed("/public", rules); got {
		t.Error("an invalid regex matched")
	}
}

// TestIsDisallowedAndValidateLinksAgree pins the two copies of the same rule
// engine together. They used to diverge: the private one had no empty-rule
// guard, so ValidateLinks and IsDisallowed returned different answers for the
// same input.
func TestIsDisallowedAndValidateLinksAgree(t *testing.T) {
	rules := []string{"", "/admin/", "^/secret/"}

	for _, path := range []string{"/", "/public", "/admin/x", "/secret/y"} {
		t.Run(path, func(t *testing.T) {
			direct := IsDisallowed(path, rules)
			viaValidate := len(ValidateLinks([]string{"https://example.com" + path}, rules)) == 0
			if direct != viaValidate {
				t.Errorf("IsDisallowed(%q) = %v but ValidateLinks dropped it = %v",
					path, direct, viaValidate)
			}
		})
	}
}

// ── ValidateLinks ────────────────────────────────────────────────────────────

func TestValidateLinksDropsDisallowedPaths(t *testing.T) {
	got := ValidateLinks([]string{
		"https://example.com/public",
		"https://example.com/admin/users",
		"https://example.com/secret/x",
		"https://example.com/other",
	}, []string{"/admin/", "^/secret/"})

	want := []string{"https://example.com/other", "https://example.com/public"}
	if len(got) != len(want) {
		t.Fatalf("ValidateLinks() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ValidateLinks()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidateLinksDeduplicates(t *testing.T) {
	got := ValidateLinks([]string{
		"https://example.com/a",
		"https://example.com/a",
		"https://example.com/a",
	}, nil)

	if len(got) != 1 {
		t.Errorf("ValidateLinks() = %v, want one entry", got)
	}
}

func TestValidateLinksDropsUnparseableLinks(t *testing.T) {
	got := ValidateLinks([]string{
		"https://example.com/good",
		"://bad",
		"ht tp://example.com/x",
	}, nil)

	if len(got) != 1 || got[0] != "https://example.com/good" {
		t.Errorf("ValidateLinks() = %v, want only the good link", got)
	}
}

func TestValidateLinksOnEmptyInput(t *testing.T) {
	if got := ValidateLinks(nil, []string{"/admin/"}); len(got) != 0 {
		t.Errorf("ValidateLinks(nil) = %v, want an empty slice", got)
	}
}

func TestValidateLinksIsDeterministic(t *testing.T) {
	// The result is a map range, so a frontier built from it would pick a
	// different set of pages on every process start once the budget bit.
	links := []string{
		"https://example.com/a", "https://example.com/b", "https://example.com/c",
		"https://example.com/d", "https://example.com/e", "https://example.com/f",
	}

	first := ValidateLinks(links, nil)
	for i := 0; i < 200; i++ {
		got := ValidateLinks(links, nil)
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d: position %d = %q, want %q\n got %v\nwant %v",
					i, j, got[j], first[j], got, first)
			}
		}
	}
}
