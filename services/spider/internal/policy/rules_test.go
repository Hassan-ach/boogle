package policy

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

func TestIsEnglish(t *testing.T) {
	tests := []struct {
		lang string
		want bool
	}{
		{"", true},
		{"   ", true},

		{"en", true},
		{"EN", true},
		{"en-US", true},
		{"en-GB", true},
		{"en_US", true},
		{"en-US-oxendict", true},
		{"  en-GB  ", true},
		{"eng", true},

		{"fr", false},
		{"de", false},
		{"es", false},
		{"ar", false},
		{"ru", false},
		{"zh-Hans", false},
		{"ja", false},
		{"FR", false},
		{"DE", false},
		{"enochian", false},
		{"english", false},
	}

	for _, tc := range tests {
		if got := IsEnglish(tc.lang); got != tc.want {
			t.Errorf("IsEnglish(%q) = %v, want %v", tc.lang, got, tc.want)
		}
	}
}

func TestIsEnglishCoversWhatTheParserFound(t *testing.T) {
	cases := map[string]bool{
		"de":         false,
		"de-DE":      false,
		"pt-BR":      false,
		"en":         true,
		"en-AU":      true,
		"":           true,
		"nl-NL":      false,
		"fr-CA":      false,
		"es-419":     false,
		"zh-Hant-TW": false,
	}
	for lang, want := range cases {
		if got := IsEnglish(lang); got != want {
			t.Errorf("IsEnglish(%q) = %v, want %v", lang, got, want)
		}
	}
}

func TestSkipsPathMatchesWholeSegments(t *testing.T) {
	r := DefaultRules()

	skipped := []string{
		"/login",
		"/login/",
		"/login/form",
		"/account/settings",
		"/cart/checkout",
	}
	for _, p := range skipped {
		if !r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = false, want true", p)
		}
	}

	kept := []string{
		"/",
		"",
		"/LOGIN",
		"/cartoon",
		"/cartography",
		"/searching",
		"/search-archive",
		"/logins-archive",
		"/administration",
		"/administrators-guide",
		"/blog/post-about-login",
		"/articles/checkout-flow",
		"/errors",
	}
	for _, p := range kept {
		if r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = true, want false", p)
		}
	}
}

func TestRobotsRulesSpecificityUsesTheRuleAsWritten(t *testing.T) {
	r := NewRobotsRules(
		[]string{"/a"},
		[]string{"/a/"},
	)

	for _, path := range []string{"/a", "/a/"} {
		allowed, rule := r.Allows(path)
		if allowed {
			t.Errorf("Allows(%q) = true (decided by %q), want false: the longer "+
				"rule as written is the disallow", path, rule)
		}
		if rule != "/a/" {
			t.Errorf("Allows(%q) was decided by %q, want the rule as written, %q",
				path, rule, "/a/")
		}
	}

	r = NewRobotsRules([]string{"/a/"}, []string{"/a"})
	if allowed, rule := r.Allows("/a"); !allowed {
		t.Errorf("Allows(\"/a\") = false (decided by %q), want true: the longer "+
			"rule as written is now the allow", rule)
	}
}

func TestSkipsPathIgnoresEmptyEntries(t *testing.T) {
	r := RuleSet{DisallowPathPrefixes: []string{"", "/", "//", "  "}}

	for _, p := range []string{"/", "/a", "/a/b/c", "/article"} {
		if r.SkipsPath(p) {
			t.Errorf("SkipsPath(%q) = true with an empty rule in the table", p)
		}
	}
}

func TestSkipsExtensionIsCaseInsensitive(t *testing.T) {
	r := DefaultRules()

	for _, p := range []string{
		"/report.pdf", "/Report.PDF", "/REPORT.Pdf",
		"/deep/path/file.DOCX",
		"/img.PNG",
		"/static/app.JS",
	} {
		if !r.SkipsExtension(p) {
			t.Errorf("SkipsExtension(%q) = false, want true", p)
		}
	}

	for _, p := range []string{
		"/article",
		"/v1.2/article",
		"/report.pdf/page",
		"/pdf",
		"/a.jsp?x=1",
		"/style.css.map",
	} {
		if r.SkipsExtension(p) {
			t.Errorf("SkipsExtension(%q) = true, want false", p)
		}
	}
}

func TestSkipsWikiLanguageSubpageRequiresBothConditions(t *testing.T) {
	r := DefaultRules()

	skipped := []string{
		"/wiki/Template:Infobox/fr/",
		"/wiki/Template:Infobox/fr",
		"/wiki/Help:Contents/de",
		"/wiki/Manual:Style/en",
		"/wiki/Extension:Something/zh-yue",
		"/wiki/Template:Infobox/doc/fr/sub",
	}
	for _, p := range skipped {
		if !r.SkipsWikiLanguageSubpage(p) {
			t.Errorf("SkipsWikiLanguageSubpage(%q) = false, want true", p)
		}
	}

	kept := []string{
		"/wiki/Paris/fr",
		"/wiki/Paris/fr/",
		"/wiki/Template:Infobox",
		"/wiki/Help:Contents",
		"/articles/how-to-choose-fr",
		"/wiki/Template:Infobox/format",
	}
	for _, p := range kept {
		if r.SkipsWikiLanguageSubpage(p) {
			t.Errorf("SkipsWikiLanguageSubpage(%q) = true, want false", p)
		}
	}
}

func TestFoldsLanguageSubdomainOnlyOnKnownDomains(t *testing.T) {
	r := DefaultRules()

	cases := []struct{ in, want string }{
		{"fr.wikipedia.org", "en.wikipedia.org"},
		{"de.wikibooks.org", "en.wikibooks.org"},
		{"zh-min-nan.wikivoyage.org", "en.wikivoyage.org"},
		{"en.wikipedia.org", "en.wikipedia.org"},
		{"wikipedia.org", "wikipedia.org"},
		{"www.fr.wikipedia.org", "en.wikipedia.org"},

		{"go.example.com", "go.example.com"},
		{"it.example.co.uk", "it.example.co.uk"},
		{"FR.EXAMPLE.COM", "fr.example.com"},
		{"blog.example.com", "blog.example.com"},
	}

	for _, tc := range cases {
		if got := r.FoldsLanguageSubdomain(tc.in); got != tc.want {
			t.Errorf("FoldsLanguageSubdomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRuleSetZeroMeansDefaultRules(t *testing.T) {
	m, _ := newTestManager(t)

	if got := m.rules(); got.isZero() {
		t.Fatal("the manager's default rules read as empty")
	}

	var zero Config
	m2 := New(zero, NewMemoryState(), testLogger())
	if r := m2.rules(); r.isZero() {
		t.Error("a zero Config produced no rules; every skip rule would be disabled")
	} else if !r.SkipsPath("/login") {
		t.Error("a zero Config lost the shipped path rules")
	}

	var permissive Config
	permissive.Rules = RuleSet{
		DisallowPathPrefixes: []string{},
		SkipFileExtensions:   []string{},
		WikiNoisyNamespaces:  []string{},
	}
	m3 := New(permissive, NewMemoryState(), testLogger())
	if m3.rules().SkipsPath("/login") {
		t.Error("an explicitly emptied rule list was replaced by the defaults")
	}
}

func TestRobotsRulesPreferTheLongestMatch(t *testing.T) {
	r := NewRobotsRules(
		[]string{"/wiki/", "/public", "/ab*"},
		[]string{"/", "/wiki/private/", "/public/secret", "/abc"},
	)

	cases := []struct {
		path      string
		wantAllow bool
		rule      string
	}{
		{"/wiki/Article", true, "/wiki/"},
		{"/public/page", true, "/public"},
		{"/wiki/private/Notes", false, "/wiki/private/"},
		{"/public/secret/x", false, "/public/secret"},
		{"/other", false, "/"},
		{"/abc", true, "/ab*"},
		{"/", false, "/"},
	}

	for _, tc := range cases {
		allowed, rule := r.Allows(tc.path)
		if allowed != tc.wantAllow {
			t.Errorf("Allows(%q) = %v (decided by %q), want %v", tc.path, allowed, rule, tc.wantAllow)
			continue
		}
		if rule != tc.rule {
			t.Errorf("Allows(%q) decided by %q, want %q", tc.path, rule, tc.rule)
		}
	}
}

func TestRobotsRulesTreatsABlankDisallowAsNothing(t *testing.T) {
	r := NewRobotsRules(nil, []string{"/private/", ""})

	if r.Refuses("/anything") {
		t.Error("a blank Disallow rule blocked a path")
	}
	if !r.Refuses("/private/x") {
		t.Error("a real Disallow rule was ignored")
	}

	parsed := parseRobots("User-agent: *\nDisallow:\nDisallow: /x\n", "*")
	if len(parsed.Disallow) != 1 || parsed.Disallow[0] != "/x" {
		t.Errorf("Disallow = %#v, want [\"/x\"]", parsed.Disallow)
	}
}

func TestRobotsRulesArePatternsNotRegularExpressions(t *testing.T) {
	t.Run("metacharacters are literal", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{`/private.`, `/a+b`, `/x(y|z)`})

		if !r.Refuses("/private.") {
			t.Error("a literal dot did not match itself")
		}
		if r.Refuses("/privateX") {
			t.Error("a dot was treated as a regex wildcard")
		}
		if r.Refuses("/a+b") != true {
			t.Error("'a+b' did not match itself")
		}
		if r.Refuses("/aab") {
			t.Error("'+' was treated as a quantifier")
		}
		if r.Refuses("/x(y|z)") != true {
			t.Error("a group did not match itself")
		}
		if r.Refuses("/xy") {
			t.Error("an alternation was interpreted")
		}
	})

	t.Run("star is a wildcard", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/a/*/c"})

		if !r.Refuses("/a/b/c") {
			t.Error("'*' did not match a single segment")
		}
		if !r.Refuses("/a/anything/deeply/c") {
			t.Error("'*' did not match a run of segments")
		}
		if r.Refuses("/a/b/d") {
			t.Error("'*' matched the wrong tail")
		}
	})

	t.Run("trailing dollar anchors", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/exact$"})

		if !r.Refuses("/exact") {
			t.Error("an anchored rule did not match")
		}
		if r.Refuses("/exact/more") {
			t.Error("'$' did not anchor the end")
		}
	})

	t.Run("a trailing slash also matches without it", func(t *testing.T) {
		r := NewRobotsRules([]string{"/wiki/"}, []string{"/"})

		for _, p := range []string{"/wiki", "/wiki/", "/wiki/Article"} {
			if r.Refuses(p) {
				t.Errorf("'Allow: /wiki/' did not match %s", p)
			}
		}
		if !r.Refuses("/wikipedia") {
			t.Error("'Allow: /wiki/' matched /wikipedia")
		}
	})

	t.Run("a bare slash still excludes everything", func(t *testing.T) {
		r := NewRobotsRules(nil, []string{"/"})

		for _, p := range []string{"/", "/anything", "/deep/path"} {
			if !r.Refuses(p) {
				t.Errorf("'Disallow: /' did not exclude %s", p)
			}
		}
	})
}

func TestRobotsRulesCompileOnce(t *testing.T) {
	r := NewRobotsRules([]string{"/a", "/b"}, []string{"/c", "/d"})
	if len(r.allow) != 2 || len(r.disallow) != 2 {
		t.Fatalf("compiled %d allow and %d disallow rules, want 2 and 2", len(r.allow), len(r.disallow))
	}
	for _, c := range append(append([]compiledRule{}, r.allow...), r.disallow...) {
		if c.re == nil {
			t.Errorf("rule %q compiled to nil", c.source)
		}
		if c.weight <= 0 {
			t.Errorf("rule %q has weight %d; specificity cannot be measured", c.source, c.weight)
		}
	}
}

func TestParseRobots(t *testing.T) {
	const body = `
# a comment on its own line
User-agent: *
Disallow: /private/
Disallow: /private/   # a trailing comment on a directive
Allow: /private/public/
Crawl-delay: 5

User-agent: BoogleBot
Disallow: /boogle-only/

Sitemap: https://example.com/sitemap.xml
Sitemap: https://example.com/news.xml
Sitemap: https://example.com/sitemap.xml
`

	got := parseRobots(body, "BoogleBot")

	wantDisallow := []string{"/private/", "/boogle-only/"}
	if !equalStrings(got.Disallow, wantDisallow) {
		t.Errorf("Disallow = %#v, want %#v", got.Disallow, wantDisallow)
	}
	if want := []string{"/private/public/"}; !equalStrings(got.Allow, want) {
		t.Errorf("Allow = %#v, want %#v", got.Allow, want)
	}
	if want := []string{"https://example.com/sitemap.xml", "https://example.com/news.xml"}; !equalStrings(got.SiteMaps, want) {
		t.Errorf("SiteMaps = %#v, want %#v", got.SiteMaps, want)
	}
	if got.CrawlDelay != 5 {
		t.Errorf("CrawlDelay = %d, want 5", got.CrawlDelay)
	}
}

func TestParseRobotsMergesEveryMatchingGroup(t *testing.T) {
	t.Run("consecutive agents share the rules below them", func(t *testing.T) {
		got := parseRobots("User-agent: *\nUser-agent: BoogleBot\nDisallow: /x\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("a named agent we do not match contributes nothing", func(t *testing.T) {
		got := parseRobots("User-agent: googlebot\nDisallow: /\n\nUser-agent: *\nDisallow: /x\n", "BoogleBot")
		if equalStrings(got.Disallow, []string{"/"}) {
			t.Error("another crawler's rules were applied to us")
		}
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("a shared stanza applies through whichever agent we match", func(t *testing.T) {
		got := parseRobots("User-agent: *\nUser-agent: googlebot\nDisallow: /\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/"}) {
			t.Errorf("Disallow = %#v, want [\"/\"]: a stanza shared with another "+
				"crawler has to reach us through the wildcard", got.Disallow)
		}

		got = parseRobots("User-agent: googlebot\nUser-agent: *\nDisallow: /a\n", "BoogleBot")
		if !equalStrings(got.Disallow, []string{"/a"}) {
			t.Errorf("Disallow = %#v, want [\"/a\"]", got.Disallow)
		}
	})

	t.Run("a wildcard group applies to everyone", func(t *testing.T) {
		got := parseRobots("User-agent: *\nDisallow: /x\n", "SomeOtherBot/2.0")
		if !equalStrings(got.Disallow, []string{"/x"}) {
			t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
		}
	})

	t.Run("sitemaps belong to no agent", func(t *testing.T) {
		got := parseRobots("Sitemap: https://example.com/s.xml\nUser-agent: googlebot\n", "BoogleBot")
		if !equalStrings(got.SiteMaps, []string{"https://example.com/s.xml"}) {
			t.Errorf("SiteMaps = %#v, want the sitemap before any agent line", got.SiteMaps)
		}
	})
}

func TestParseRobotsIgnoresRulesBeforeAnyAgent(t *testing.T) {
	got := parseRobots("Disallow: /orphan\nUser-agent: *\nDisallow: /real\n", "*")
	if equalStrings(got.Disallow, []string{"/orphan", "/real"}) {
		t.Error("a rule belonging to no agent was applied")
	}
	if !equalStrings(got.Disallow, []string{"/real"}) {
		t.Errorf("Disallow = %#v, want [\"/real\"]", got.Disallow)
	}
}

func TestParseRobotsTakesTheLargestCrawlDelay(t *testing.T) {
	got := parseRobots("User-agent: *\nCrawl-delay: 2\n\nUser-agent: BoogleBot\nCrawl-delay: 9\n", "BoogleBot")
	if got.CrawlDelay != 9 {
		t.Errorf("CrawlDelay = %d, want 9", got.CrawlDelay)
	}
}

func TestParseRobotsDedupes(t *testing.T) {
	got := parseRobots(
		"User-agent: *\nDisallow: /x\nSitemap: https://e/s.xml\n\n"+
			"User-agent: BoogleBot\nDisallow: /x\nSitemap: https://e/s.xml\n",
		"BoogleBot")
	if len(got.Disallow) != 1 {
		t.Errorf("Disallow = %#v, want one entry", got.Disallow)
	}
	if len(got.SiteMaps) != 1 {
		t.Errorf("SiteMaps = %#v, want one entry", got.SiteMaps)
	}
}

func TestParseRobotsOnEmptyInput(t *testing.T) {
	for _, body := range []string{"", "\n\n\n", "# nothing but comments\n"} {
		got := parseRobots(body, "BoogleBot")
		if got == nil {
			t.Fatalf("parseRobots(%q) = nil", body)
		}
		if len(got.Allow) != 0 || len(got.Disallow) != 0 || len(got.SiteMaps) != 0 {
			t.Errorf("parseRobots(%q) = %#v, want empty", body, got)
		}
		if NewRobotsRules(got.Allow, got.Disallow).Refuses("/x") {
			t.Errorf("parseRobots(%q) refused a path; no rules means no restriction", body)
		}
	}
}

func TestParseRobotsKeepsEveryCaseOfADirective(t *testing.T) {
	got := parseRobots("USER-AGENT: *\nDISALLOW: /x\nAllow: /y\ncRaWl-DeLaY: 3\nSITEMAP: https://e/s.xml\n", "*")

	if !equalStrings(got.Disallow, []string{"/x"}) {
		t.Errorf("Disallow = %#v, want [\"/x\"]", got.Disallow)
	}
	if !equalStrings(got.Allow, []string{"/y"}) {
		t.Errorf("Allow = %#v, want [\"/y\"]", got.Allow)
	}
	if got.CrawlDelay != 3 {
		t.Errorf("CrawlDelay = %d, want 3", got.CrawlDelay)
	}
	if !equalStrings(got.SiteMaps, []string{"https://e/s.xml"}) {
		t.Errorf("SiteMaps = %#v, want the sitemap", got.SiteMaps)
	}
}

func TestSplitDirective(t *testing.T) {
	cases := []struct {
		line, key, value string
		ok               bool
	}{
		{"Disallow: /x", "disallow", "/x", true},
		{"  Allow :   /y  ", "allow", "/y", true},
		{"Sitemap: https://e/s.xml", "sitemap", "https://e/s.xml", true},
		{"Disallow:/nospace", "disallow", "/nospace", true},
		{"Disallow: /a:b", "disallow", "/a:b", true},
		{"no colon here", "", "", false},
		{": leading colon", "", "", false},
	}

	for _, tc := range cases {
		key, value, ok := splitDirective(tc.line)
		if ok != tc.ok || key != tc.key || value != tc.value {
			t.Errorf("splitDirective(%q) = %q/%q/%v, want %q/%q/%v",
				tc.line, key, value, ok, tc.key, tc.value, tc.ok)
		}
	}
}

func TestTheRuleTablesAreTheOnlyCopy(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"example.com": {body: ""}},
		status: 200,
	}).fetcher())
	r := DefaultRules()

	for _, path := range ruleCorpus(r) {
		full := "https://example.com" + path
		if path == "" {
			full = "https://example.com/"
		}
		canon, ok := utils.CanonicalizeUrl(full, "example.com")
		if !ok {
			t.Errorf("CanonicalizeUrl(%q) rejected the URL, so this case is testing "+
				"canonicalisation rather than the rule tables", full)
			continue
		}
		u, err := url.Parse(canon)
		if err != nil {
			t.Fatalf("parse %q: %v", canon, err)
		}

		wantRefused := r.SkipsPath(u.Path) ||
			r.SkipsExtension(u.Path) ||
			r.SkipsWikiLanguageSubpage(u.Path)
		v := mustAdmit(t, m, canon)
		if wantRefused && v.Kind == Allow {
			t.Errorf("path %q: the rule tables refuse it but Admit allows it", path)
		}
		if !wantRefused && v.Kind != Allow {
			t.Errorf("path %q: Admit refused it for %q, but no rule covers it",
				path, v.Reason)
		}
	}
}

func TestEveryRefusedPathCarriesAReason(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"example.com": {body: ""}},
		status: 200,
	}).fetcher())
	r := DefaultRules()

	ruleReasons := map[Reason]bool{
		ReasonPathDisallowed:     true,
		ReasonExtensionSkipped:   true,
		ReasonLanguageNotEnglish: true,
	}

	for _, path := range ruleCorpus(r) {
		full := "https://example.com" + path
		if path == "" {
			full = "https://example.com/"
		}
		canon, ok := utils.CanonicalizeUrl(full, "example.com")
		if !ok {
			continue
		}
		u, err := url.Parse(canon)
		if err != nil {
			t.Fatalf("parse %q: %v", canon, err)
		}
		if !r.SkipsPath(u.Path) && !r.SkipsExtension(u.Path) && !r.SkipsWikiLanguageSubpage(u.Path) {
			continue
		}

		v := mustAdmit(t, m, canon)
		if v.Reason == "" {
			t.Errorf("path %q was refused with no reason, so it is counted under "+
				"the empty string", path)
			continue
		}
		if !ruleReasons[v.Reason] {
			t.Errorf("path %q was refused for %q, want one of the rule reasons %v",
				path, v.Reason, ruleReasons)
		}
	}

	for _, reason := range []Reason{ReasonPathDisallowed, ReasonExtensionSkipped} {
		if got := st.Stats("example.com")[reason]; got == 0 {
			t.Errorf("no refusal counted under %q after admitting the corpus", reason)
		}
	}
}

func ruleCorpus(r RuleSet) []string {
	seen := map[string]struct{}{}
	out := []string{}

	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	for _, suffix := range []string{"", "/", "/x", "/x/y", "oon", "oon/x", "-archive", "s", "/page"} {
		for _, entry := range r.DisallowPathPrefixes {
			add(entry + suffix)
			add(entry + strings.ToUpper(suffix))
		}
		for _, entry := range r.SkipFileExtensions {
			add("/dir" + entry + suffix)
			add("/dir" + strings.ToUpper(entry) + suffix)
		}
		for _, ns := range r.WikiNoisyNamespaces {
			for _, lang := range []string{"/fr", "/fr/", "/de", "/zh-yue", "/fr/sub", "", "/france"} {
				add("/wiki" + ns + "Infobox" + lang)
			}
		}
	}

	for _, p := range []string{
		"/", "", "/article", "/blog/2026/a-post", "/wiki/Paris",
		"/docs/getting-started", "/v1.2/api", "/user/settings-not-ours",
	} {
		add(p)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
