package contract

import (
	"strings"
	"testing"
)

func TestNormIdent(t *testing.T) {
	// The three spellings of one field must collapse onto one key, which is
	// what lets a trace follow a value across a language boundary.
	for _, s := range []string{"UserId", "user_id", "userId", " user_id "} {
		if got := NormIdent(s); got != "userid" {
			t.Errorf("NormIdent(%q) = %q, want %q", s, got, "userid")
		}
	}
}

func TestSplitTopLevel(t *testing.T) {
	tests := []struct {
		in   string
		sep  byte
		want []string
	}{
		{"a, b", ',', []string{"a", " b"}},
		// A generic argument list is one part, not two.
		{"Map<String, Int> m, int n", ',', []string{"Map<String, Int> m", " int n"}},
		{"f(a, b), c", ',', []string{"f(a, b)", " c"}},
		// A comparison is not a generic: the depth must not go negative or stick.
		{"a < b, c > d", ',', []string{"a < b", " c > d"}},
	}
	for _, tt := range tests {
		got := SplitTopLevel(tt.in, tt.sep)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("SplitTopLevel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParamNames(t *testing.T) {
	tests := []struct {
		name string
		lang string
		sig  string
		want []string
	}{
		{"go", "go", "(userID string, n int)", []string{"userID", "n"}},
		{"go variadic", "go", "(args ...string)", []string{"args"}},
		{"typescript optional", "typescript", "(uri?: string, qs: object)", []string{"uri", "qs"}},
		{"java type first", "java", "(String userId, final int n)", []string{"userId", "n"}},
		{"csharp attribute", "csharp", "([FromBody] OrderDto order)", []string{"order"}},
		{"java generic arg", "java", "(Map<String, Int> m, int n)", []string{"m", "n"}},
		{"default value", "typescript", "(amount: number = 0)", []string{"amount"}},
		// A Python bound method lists the receiver but never receives it as an
		// argument, so index 0 must line up with the first real parameter.
		{"python self", "python", "(self, user_id)", []string{"user_id"}},
		{"python cls", "python", "(cls, user_id)", []string{"user_id"}},
		{"empty", "go", "()", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParamNames(tt.lang, tt.sig)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("ParamNames(%q, %q) = %q, want %q", tt.lang, tt.sig, got, tt.want)
			}
		})
	}
}
