package utils

import (
	"net/url"
	"strings"
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

// TestNormalizeUrlDropsPaginationAndSearchParams spells the list out rather than
// reading it back from the table it is checking.
//
// It read it back, once. A test that iterates the exclusion table asserts only that
// the parameters in it are excluded, which is true of any table including an empty
// one -- so the mutation harness deleted four entries and this test went on passing,
// having checked "sort" three hundred times. Spelling the list out makes the test
// the specification and the table the implementation, which is the only arrangement
// in which removing an entry is a failure rather than a quieter crawl.
//
// /p?page=1 through /p?page=50000 are one page, and a site search result set is an
// infinite crawl. Each entry is a way a site says "this is a different document"
// when it is not, and a parameter that quietly stops being excluded is not a slow
// leak: it is a crawl with no end, reached through ordinary links, reported by
// nothing.
func TestNormalizeUrlDropsPaginationAndSearchParams(t *testing.T) {
	for _, param := range []string{"sort", "page", "filter", "q", "search"} {
		t.Run(param, func(t *testing.T) {
			got, ok := NormalizeUrl("http://example.com/p?"+param+"=3&keep=1", "")
			if !ok {
				t.Fatal("rejected")
			}
			if got != "https://example.com/p?keep=1" {
				t.Errorf("NormalizeUrl() = %q, want %q dropped", got, param)
			}
		})
	}

	if got := strings.Join(disallowQueryParams, ","); got != "sort,page,filter,q,search" {
		t.Errorf("disallowQueryParams = %q, want sort,page,filter,q,search", got)
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

// TestCanonicalizeUrlKeepsWhatTheRulesUsedToRefuse pins the move.
//
// These four tests used to assert that NormalizeUrl refused /login, /cart,
// /file.pdf and /wiki/Template:Foo/fr. They do not any more, and that is the
// point of the change rather than a loss of coverage: the skip tables were a
// second copy of policy.RuleSet, applied inside canonicalisation, where a refusal
// could not be counted and its reason had nowhere to go. A caller asking "what URL
// is this?" got the answer "none, and by the way it was a PDF", and the second
// half of that was the only part anyone could not have predicted.
//
// So canonicalisation now returns all of them, and policy.Admit refuses them with
// a reason. The tests that assert they are still refused live in
// policy/rules_tables_test.go, where the tables are. This one exists to catch the
// other failure mode, which is silent and expensive: a table entry dropped during
// the move. Nothing would complain -- the URL would reach the frontier, be
// fetched, be indexed, and no log anywhere would say it should not have been.
func TestCanonicalizeUrlKeepsWhatTheRulesUsedToRefuse(t *testing.T) {
	for _, raw := range []string{
		// The path table.
		"http://example.com/login",
		"http://example.com/admin/users",
		"http://example.com/cart",
		"http://example.com/search",
		"http://example.com/settings/profile",
		"http://example.com/404",
		// The extension table.
		"http://example.com/file.pdf",
		"http://example.com/file.jpg",
		"http://example.com/file.mp4",
		"http://example.com/FILE.PDF",
		"http://example.com/style.css",
		// The wiki-namespace table.
		"http://en.wikipedia.org/wiki/Template:Foo/fr",
		"http://en.wikipedia.org/wiki/Help:Bar/en",
		"http://en.wikipedia.org/wiki/Manual:Baz/en",
		"http://en.wikipedia.org/wiki/Extension:Qux/de",
	} {
		t.Run(raw, func(t *testing.T) {
			got, ok := CanonicalizeUrl(raw, "")
			if !ok {
				t.Fatalf("CanonicalizeUrl(%q) refused the URL; the skip rules "+
					"belong to policy.Admit, which can count them", raw)
			}
			if got == "" {
				t.Errorf("CanonicalizeUrl(%q) returned an empty URL with ok=true", raw)
			}
		})
	}
}

// TestNormalizeUrlIsCanonicalizeUrl pins the deprecated wrapper.
//
// The wrapper exists for one release so an out-of-tree caller does not break, and
// its whole contract is that it is the same function. A wrapper that quietly kept
// the old skip behaviour would be worse than no wrapper: a deployment would go on
// dropping URLs before the policy manager saw them, so the reasons would still go
// uncounted while the code read as though the fix had shipped.
func TestNormalizeUrlIsCanonicalizeUrl(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/login",
		"http://example.com/file.pdf",
		"http://en.wikipedia.org/wiki/Template:Foo/fr",
		"http://example.com/a",
		"#fragment",
		"ht tp://example.com/x",
	} {
		wantURL, wantOK := CanonicalizeUrl(raw, "")
		gotURL, gotOK := NormalizeUrl(raw, "")
		if gotURL != wantURL || gotOK != wantOK {
			t.Errorf("NormalizeUrl(%q) = (%q, %v), CanonicalizeUrl = (%q, %v); "+
				"the deprecated wrapper must be the same function", raw,
				gotURL, gotOK, wantURL, wantOK)
		}
	}
}

// TestNormalizeUrlKeepsOrdinaryPathsThatMerelyStartLikeADisallowedOne is now a
// statement about identity rather than about rules.
//
// These are ordinary pages that happen to share a few leading characters with a
// path in the skip table, and canonicalisation has no table and no opinion, so it
// keeps them. The interesting version of this test -- that the *rule* matching
// does not eat them either, because it matches whole path segments and not raw
// prefixes -- is TestSkipsPathMatchesWholeSegments in the policy package, next to
// the table it is about. Duplicating it here would be duplicating the rule.

// A download endpoint is not necessarily a binary. The extension check runs on
// the path, so ?file=a.pdf survives normalisation on purpose: the path has no
// extension, and guessing at query values would drop real HTML pages whose URLs
// happen to carry a "file" parameter. Documented rather than asserted either way.
func TestCanonicalizeUrlKeepsOrdinaryPathsThatMerelyStartLikeADisallowedOne(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/cartoon",
		"http://example.com/searching",
		"http://example.com/logins-archive",
		"http://example.com/settings-overview",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, ok := CanonicalizeUrl(raw, ""); !ok {
				t.Errorf("CanonicalizeUrl(%q) was rejected", raw)
			}
		})
	}
}

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
	// The rejected entries are now only the ones that are not URLs. A /login or a
	// .pdf in a batch is a URL the policy manager will refuse with a reason, not
	// one this function gets to silently omit.
	got := NormalizeUrls([]string{
		"http://example.com/a",
		"http://example.com/login",
		"http://example.com/b",
		"#fragment",
		"http://example.com/c.pdf",
	}, "")

	want := []string{
		"https://example.com/a",
		"https://example.com/login",
		"https://example.com/b",
		"https://example.com/c.pdf",
	}
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
	got := NormalizeUrls([]string{"#a", "ht tp://example.com/x", "#b"}, "")
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
