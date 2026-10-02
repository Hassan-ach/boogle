package policy

import (
	"net/url"
	"testing"
	"time"
)

// A host with a large rule set, which is what a big site publishes.
func benchAllow() []string {
	return []string{"/private/public/", "/api/docs/"}
}

func benchDisallow() []string {
	out := make([]string, 0, 300)
	for i := 0; i < 100; i++ {
		out = append(out, "/section/", "/tag/", "/page/")
	}
	return out
}

func benchURL() *url.URL {
	u, err := url.Parse("https://example.com/private/public/article.html")
	if err != nil {
		panic(err)
	}
	return u
}

func benchManager() *PolicyManager {
	return New(DefaultConfig(), NewMemoryState(), testLogger())
}

// admitsLocally compiles a host's robots rules into regexps. A large sitemap
// runs it over thousands of entries against one HostState, so the per-call
// compile dominates. Reuses is the path that has the cache; Recompiles is the
// path a state rebuilt from Redis hands back.
func BenchmarkAdmitsLocallyReusesCompiled(b *testing.B) {
	m := benchManager()
	st := HostState{}.WithRobots("example.com", benchAllow(), benchDisallow(), nil, time.Second, time.Unix(1, 0))
	u := benchURL()

	b.ReportAllocs()
	for b.Loop() {
		if _, ok := m.admitsLocally(st, u); !ok {
			b.Fatal("the benchmark url was refused; nothing is being measured")
		}
	}
}

func BenchmarkAdmitsLocallyRecompiles(b *testing.B) {
	m := benchManager()
	st := HostState{}.WithRobots("example.com", benchAllow(), benchDisallow(), nil, time.Second, time.Unix(1, 0))
	u := benchURL()

	b.ReportAllocs()
	for b.Loop() {
		st.compiled = nil
		if _, ok := m.admitsLocally(st, u); !ok {
			b.Fatal("the benchmark url was refused; nothing is being measured")
		}
	}
}
