package util

import (
	"math"
	"testing"
)

func TestGetWithDefaultReturnsTheValueWhenSet(t *testing.T) {
	t.Setenv("BOOGLE_TEST_STR", "configured")

	if got := GetWithDefault("BOOGLE_TEST_STR", "fallback"); got != "configured" {
		t.Errorf("GetWithDefault() = %q, want %q", got, "configured")
	}
}

func TestGetWithDefaultReturnsTheDefaultWhenUnset(t *testing.T) {
	if got := GetWithDefault("BOOGLE_TEST_STR_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("GetWithDefault() = %q, want %q", got, "fallback")
	}
}

func TestGetWithDefaultTreatsAnEmptyValueAsUnset(t *testing.T) {
	t.Setenv("BOOGLE_TEST_STR", "")

	if got := GetWithDefault("BOOGLE_TEST_STR", "fallback"); got != "fallback" {
		t.Errorf("GetWithDefault() = %q, want the default %q", got, "fallback")
	}
}

func TestGetWithDefaultDoesNotDistinguishAbsentFromEmpty(t *testing.T) {
	t.Setenv("BOOGLE_TEST_EMPTY", "")
	t.Setenv("BOOGLE_TEST_WS", " ")

	if GetWithDefault("BOOGLE_TEST_EMPTY", "d") != GetWithDefault("BOOGLE_TEST_ABSENT", "d") {
		t.Error("an empty value and an absent value must resolve the same way")
	}
	if got := GetWithDefault("BOOGLE_TEST_WS", "d"); got != " " {
		t.Errorf("GetWithDefault() = %q, want the whitespace value to be kept", got)
	}
}

func TestGetIntWithDefault(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  int
	}{
		{"parses an integer", true, "42", 42},
		{"parses zero", true, "0", 0},
		{"parses a negative", true, "-7", -7},
		{"parses a leading plus", true, "+7", 7},
		{"falls back on a hex literal", true, "0x10", 99},
		{"falls back on an octal literal", true, "0o10", 99},
		{"falls back when unset", false, "", 99},
		{"falls back when empty", true, "", 99},
		{"falls back on a non-number", true, "abc", 99},
		{"falls back on a float", true, "4.2", 99},
		{"falls back on trailing junk", true, "42abc", 99},
		{"falls back on surrounding space", true, "  42  ", 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("BOOGLE_TEST_INT", tc.value)
			}
			if got := GetIntWithDefault("BOOGLE_TEST_INT", 99); got != tc.want {
				t.Errorf("GetIntWithDefault(%q, 99) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestGetFloatWithDefault(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  float64
	}{
		{"parses a float", true, "4.2", 4.2},
		{"parses a whole number", true, "4", 4},
		{"parses a negative", true, "-0.5", -0.5},
		{"parses exponent notation", true, "1e3", 1000},
		{"parses a trailing dot", true, "4.", 4},
		{"parses a leading dot", true, ".5", 0.5},
		{"parses infinity", true, "Inf", math.Inf(1)},
		{"falls back when unset", false, "", 1.5},
		{"falls back when empty", true, "", 1.5},
		{"falls back on a non-number", true, "abc", 1.5},
		{"falls back on a bare dot", true, ".", 1.5},
		{"falls back on surrounding space", true, "  4.2  ", 1.5},
		{"falls back on NaN", true, "NaN", 1.5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("BOOGLE_TEST_FLOAT", tc.value)
			}
			if got := GetFloatWithDefault("BOOGLE_TEST_FLOAT", 1.5); got != tc.want {
				t.Errorf("GetFloatWithDefault(%q, 1.5) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestGetIntAndGetFloatAgreeOnWholeNumbers(t *testing.T) {
	t.Setenv("BOOGLE_TEST_BOTH", "2")

	if got := GetIntWithDefault("BOOGLE_TEST_BOTH", -1); got != 2 {
		t.Errorf("GetIntWithDefault() = %d, want 2", got)
	}
	if got := GetFloatWithDefault("BOOGLE_TEST_BOTH", -1); got != 2 {
		t.Errorf("GetFloatWithDefault() = %v, want 2", got)
	}
}

func TestDefaultsAreUsedIndependently(t *testing.T) {
	t.Setenv("BOOGLE_TEST_GOOD", "10")
	t.Setenv("BOOGLE_TEST_BAD", "not-a-number")

	if got := GetIntWithDefault("BOOGLE_TEST_GOOD", 1); got != 10 {
		t.Errorf("GetIntWithDefault(good) = %d, want 10", got)
	}
	if got := GetIntWithDefault("BOOGLE_TEST_BAD", 1); got != 1 {
		t.Errorf("GetIntWithDefault(bad) = %d, want the default 1", got)
	}
	if got := GetIntWithDefault("BOOGLE_TEST_ABSENT", 1); got != 1 {
		t.Errorf("GetIntWithDefault(absent) = %d, want the default 1", got)
	}
}
