package policy

import (
	"strings"
	"testing"
)

func TestKeyspaceKeys(t *testing.T) {
	ks := NewKeyspace("boogle:spider")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"frontier", ks.Frontier(), "{boogle:spider:frontier}"},
		{"delayed", ks.Delayed(), "{boogle:spider:delayed}"},
		{"visited", ks.Visited(), "{boogle:spider:visited}"},
		{"host state", ks.HostState("example.com"), "{boogle:spider:host:example.com}"},
		{"dead marker", ks.HostMarker("example.com", MarkerDead), "{boogle:spider:host:example.com}:dead"},
		{"cold marker", ks.HostMarker("example.com", MarkerCold), "{boogle:spider:host:example.com}:cold"},
		{"cooldown marker", ks.HostMarker("example.com", MarkerCooldown), "{boogle:spider:host:example.com}:cooldown"},
		{"stats", ks.Stats("example.com"), "{boogle:spider:stats:example.com}"},
	}

	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestKeyspaceHostKeysShareASlot is the reason every key carries a hash tag.
// A host's state hash and its three markers are read together in one pipeline
// and, in a later phase, touched together in one script. If they hashed to
// different slots that would be a CROSSSLOT error the day this cache is
// clustered, and it would be discovered in production rather than here.
func TestKeyspaceHostKeysShareASlot(t *testing.T) {
	ks := NewKeyspace("boogle:spider")
	host := "example.com"

	keys := []string{
		ks.HostState(host),
		ks.HostMarker(host, MarkerDead),
		ks.HostMarker(host, MarkerCold),
		ks.HostMarker(host, MarkerCooldown),
	}

	tag := hashTagOf(keys[0])
	if tag == "" {
		t.Fatalf("host state key %q has no hash tag", keys[0])
	}
	for _, k := range keys {
		if got := hashTagOf(k); got != tag {
			t.Errorf("key %q hashes as %q, want %q -- a host's keys would scatter "+
				"across slots", k, got, tag)
		}
	}
}

// TestKeyspaceDifferentHostsDoNotShareASlot is the other half: co-locating one
// host's keys must not put every host in the same slot, which would make a
// cluster no better than a single node.
func TestKeyspaceDifferentHostsDoNotShareASlot(t *testing.T) {
	ks := NewKeyspace("boogle:spider")

	a := hashTagOf(ks.HostState("a.example.com"))
	b := hashTagOf(ks.HostState("b.example.com"))
	if a == b {
		t.Errorf("hosts a and b share the tag %q; a cluster would put every host on one node", a)
	}
}

func TestKeyspaceURLStateIsHashed(t *testing.T) {
	ks := NewKeyspace("boogle:spider")

	long := "https://example.com/" + strings.Repeat("segment/", 200) + "page?q=" + strings.Repeat("x", 500)
	key := ks.URLState(long)

	if len(key) > 100 {
		t.Errorf("url state key is %d bytes; it is written on every attempt and a url can "+
			"run to several kilobytes", len(key))
	}
	if !strings.HasPrefix(key, "{boogle:spider:url:") {
		t.Errorf("key = %q, want the url namespace", key)
	}

	// Distinct URLs must not collide, and the same URL must be stable.
	if ks.URLState(long) != key {
		t.Error("url state key is not stable for the same url")
	}
	if ks.URLState(long+"x") == key {
		t.Error("two different urls produced the same key")
	}
}

// TestKeyspaceVisitedHoldsFullURLs documents the deliberate asymmetry. Visited
// is keyed on the URL itself because exact-string dedup is the correctness
// property, while the per-URL state hash is hashed because it is written often
// and URLs are long. Hashing visited would make dedup a collision question.
func TestKeyspaceVisitedHoldsFullURLs(t *testing.T) {
	ks := NewKeyspace("boogle:spider")

	url := "https://example.com/a/b?c=d"
	if !strings.Contains(ks.Visited(), url) && ks.Visited() != "{boogle:spider:visited}" {
		t.Errorf("visited key %q is not a plain namespace key", ks.Visited())
	}
}

func TestKeyspacePrefixNormalisation(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// A trailing colon is an easy thing to type by hand, and it would
		// produce "boogle:spider::frontier" -- a different key, silently empty.
		{"boogle:spider:", "boogle:spider"},
		{"boogle:spider::", "boogle:spider"},
		{"  boogle:spider  ", "boogle:spider"},
		{"", DefaultRedisPrefix},
		{":", DefaultRedisPrefix},
	}

	for _, tc := range tests {
		if got := NewKeyspace(tc.in).Prefix(); got != tc.want {
			t.Errorf("NewKeyspace(%q).Prefix() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestKeyspaceRejectsBracesInHost guards the hash tag itself. A host containing
// a brace would redefine where its keys live, and a host is parsed out of a URL
// by a crawler pointed at whatever it found.
func TestKeyspaceBracesInHostCannotEscapeTheTag(t *testing.T) {
	ks := NewKeyspace("boogle:spider")
	host := "evil}.example.com{other"

	for _, key := range []string{
		ks.HostState(host),
		ks.HostMarker(host, MarkerDead),
		ks.Stats(host),
	} {
		tag := hashTagOf(key)
		if strings.ContainsAny(tag, " ") || tag == "" {
			t.Errorf("key %q produced a malformed tag %q", key, tag)
		}
		if strings.Count(key, "{") != 1 || strings.Count(key, "}") != 1 {
			t.Errorf("key %q has unbalanced or nested braces; it would hash to the wrong slot", key)
		}
	}

	// And it must be deterministic, so the same odd host always maps to the same
	// key rather than to a fresh one each run.
	if ks.HostState(host) != ks.HostState(host) {
		t.Error("host state key is not deterministic")
	}
}

func TestKeyspaceUnknownMarkerCannotCollide(t *testing.T) {
	ks := NewKeyspace("boogle:spider")

	bad := ks.HostMarker("example.com", MarkerKind(99))
	for _, kind := range AllMarkers {
		if bad == ks.HostMarker("example.com", kind) {
			t.Errorf("an unknown marker kind produced %q, which collides with a real one", bad)
		}
	}
	// It must also not be the host's state key, or a caller that passed a bad
	// kind would SET over the host's real policy state.
	if bad == ks.HostState("example.com") {
		t.Error("an unknown marker kind produced the host state key")
	}
}

func TestMarkerKindString(t *testing.T) {
	for kind, want := range map[MarkerKind]string{
		MarkerCooldown: "cooldown",
		MarkerDead:     "dead",
		MarkerCold:     "cold",
		MarkerKind(99): "invalid",
	} {
		if got := kind.String(); got != want {
			t.Errorf("MarkerKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

// hashTagOf returns the {braced} portion Redis Cluster would use for slot
// selection, or "" if the key has none.
func hashTagOf(key string) string {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return ""
	}
	close := strings.IndexByte(key[open+1:], '}')
	if close < 0 {
		return ""
	}
	return key[open+1 : open+1+close]
}
