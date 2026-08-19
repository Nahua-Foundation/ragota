package enrich

import (
	"strings"
	"testing"
)

func TestSanitizeRewrittenQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain output is kept", in: "login auth session", want: "login auth session"},
		{name: "surrounding double quotes", in: "\"login auth session\"", want: "login auth session"},
		{name: "surrounding single quotes", in: "'login auth'", want: "login auth"},
		{name: "backticks", in: "`login auth`", want: "login auth"},
		{name: "nested quotes", in: "\"'login auth'\"", want: "login auth"},
		{name: "label prefix", in: "Query: login auth", want: "login auth"},
		{name: "label and quotes", in: "Search query: \"login auth\"", want: "login auth"},
		{name: "quoted label", in: "\"Query: login auth\"", want: "login auth"},
		{name: "think block", in: "<think>the user wants auth</think>\nlogin auth", want: "login auth"},
		{name: "unterminated think block", in: "<think>reasoning that never ends", want: ""},
		{name: "code fence", in: "```\nlogin auth\n```", want: "login auth"},
		{name: "extra prose lines", in: "login auth\n\nThis finds the login code.", want: "login auth"},
		{name: "leading blank lines", in: "\n\n  login auth  ", want: "login auth"},
		{name: "markdown bullet", in: "- login auth", want: "login auth"},
		{name: "inner quotes are neutralised", in: `login "auth session" token`, want: "login auth session token"},
		{name: "empty", in: "   ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeRewrittenQuery(tt.in); got != tt.want {
				t.Errorf("sanitizeRewrittenQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeRewrittenQueryCapsLength(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("identifier ", 60))
	got := sanitizeRewrittenQuery(long)
	if len(got) > maxRewrittenQueryLen {
		t.Errorf("length = %d, want <= %d", len(got), maxRewrittenQueryLen)
	}
	if strings.HasSuffix(got, "identifie") {
		t.Error("query was cut mid-word")
	}
}
