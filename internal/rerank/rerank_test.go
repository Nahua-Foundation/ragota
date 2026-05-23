package rerank

import "testing"

func TestParseScore(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
	}{
		{"0.95", 0.95},
		{"Score: 0.8", 0.8},
		{"0,75", 0.75},
		{"Relevant: yes", 1.0},
		{"Not relevant", 0.0},
		{"95", 0.95},
		{"120", 1.0},
		{"-5", 0.0},
		{"abc 0.5 def", 0.5},
	}

	for _, tc := range tests {
		got := parseScore(tc.input)
		if got != tc.expected {
			t.Errorf("parseScore(%q) = %v; want %v", tc.input, got, tc.expected)
		}
	}
}
