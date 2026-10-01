package utils

import (
	"net/url"
	"strings"
	"testing"
)

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
	got, ok := NormalizeUrl("https://other.com/page", "example.com")
	if !ok {
		t.Fatal("rejected an absolute cross-host URL")
	}
	if got != "https://other.com/page" {
		t.Errorf("NormalizeUrl() = %q, want the original host preserved", got)
	}
}

func TestNormalizeUrlStripsFragments(t *testing.T) {
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

func TestCanonicalizeUrlKeepsWhatTheRulesUsedToRefuse(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/login",
		"http://example.com/admin/users",
		"http://example.com/cart",
		"http://example.com/search",
		"http://example.com/settings/profile",
		"http://example.com/404",
		"http://example.com/file.pdf",
		"http://example.com/file.jpg",
		"http://example.com/file.mp4",
		"http://example.com/FILE.PDF",
		"http://example.com/style.css",
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

func TestNormalizeUrlsDropsTheRejectedEntries(t *testing.T) {
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
	if IsDisallowed("/admin", rules) {
		t.Error("the rule /admin/ should not match the bare path /admin")
	}
}

func TestIsDisallowedTreatsRuleLikePatternsAsRegexes(t *testing.T) {
	rules := []string{`^/secret/.*`, `/admin/`, `\.php$`}

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
	rules := []string{"[unclosed", "/admin/"}

	if !IsDisallowed("/admin/x", rules) {
		t.Error("a valid rule alongside an invalid one was ignored")
	}
	if got := IsDisallowed("/public", rules); got {
		t.Error("an invalid regex matched")
	}
}

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
