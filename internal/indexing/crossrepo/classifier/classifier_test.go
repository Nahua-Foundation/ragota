package classifier

import (
	"testing"

	"ragota/pkg/repos"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── normalizeName ──

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"already normalized", "authservice", "authservice"},
		{"with hyphens", "auth-service", "authservice"},
		{"with underscores", "auth_service", "authservice"},
		{"mixed separators", "auth_service-name", "authservicename"},
		{"uppercase", "AuthService", "authservice"},
		{"mixed case with separators", "Auth-Service_Name", "authservicename"},
		{"empty", "", ""},
		{"all separators", "-_-", ""},
		{"numbers preserved", "service-v2", "servicev2"},
		{"only lowercase letters", "abc", "abc"},
		{"only uppercase letters", "ABC", "abc"},
		{"spaces preserved", "auth service", "auth service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ── toLower ──

func TestToLower(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"all uppercase", "ABC", "abc"},
		{"all lowercase", "abc", "abc"},
		{"mixed case", "AbC123", "abc123"},
		{"empty", "", ""},
		{"with hyphens", "HELLO-WORLD", "hello-world"},
		{"mixed case with separators", "MiXeD_CaSe", "mixed_case"},
		{"numbers and special", "Test123!", "test123!"},
		{"only special chars", "!@#$%", "!@#$%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toLower(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ── stringContainsFold ──

func TestStringContainsFold(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		substr   string
		expected bool
	}{
		{"exact match", "auth-service", "auth-service", true},
		{"case insensitive match", "auth-service", "AUTH", true},
		{"substring at start", "auth-service", "auth", true},
		{"substring at end", "auth-service", "service", true},
		{"substring in middle", "auth-service", "th-ser", true},
		{"full string uppercase", "auth-service", "AUTH-SERVICE", true},
		{"not found", "auth-service", "xyz", false},
		{"empty substring", "auth-service", "", true},
		{"empty string", "", "auth", false},
		{"both empty", "", "", true},
		{"single char match", "abc", "b", true},
		{"single char no match", "abc", "d", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringContainsFold(tt.s, tt.substr)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ── containsIgnoreCase ──

func TestContainsIgnoreCase(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected bool
	}{
		{"a contains b", "auth-service", "auth", true},
		{"b contains a", "auth", "auth-service", true},
		{"equal", "auth-service", "auth-service", true},
		{"no overlap", "auth-service", "database", false},
		{"empty a", "", "auth", false},
		{"empty b", "auth", "", false},
		{"both empty", "", "", false},
		{"case insensitive", "AUTH-service", "auth", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsIgnoreCase(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ── resolverForService ──

func TestResolverForService(t *testing.T) {
	tests := []struct {
		name         string
		serviceName  string
		repoNames    []string
		expectedRepo string
	}{
		{
			name:         "exact match",
			serviceName:  "auth-service",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "partial match - repo contains service",
			serviceName:  "auth",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "partial match - service contains repo",
			serviceName:  "auth-service-backend",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "normalized match - underscores vs hyphens",
			serviceName:  "auth_service",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "normalized match - hyphens vs underscores",
			serviceName:  "auth-service",
			repoNames:    []string{"auth_service", "user_service"},
			expectedRepo: "auth_service",
		},
		{
			name:         "normalized match - no separators",
			serviceName:  "authservice",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "no match",
			serviceName:  "payment-service",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "",
		},
		{
			name:         "empty service name",
			serviceName:  "",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "",
		},
		{
			name:         "case insensitive match",
			serviceName:  "AUTH-SERVICE",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "mixed case normalized",
			serviceName:  "AuthService",
			repoNames:    []string{"auth-service", "user-service"},
			expectedRepo: "auth-service",
		},
		{
			name:         "first match wins",
			serviceName:  "service",
			repoNames:    []string{"auth-service", "other-service"},
			expectedRepo: "auth-service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoList := make([]repos.Repo, len(tt.repoNames))
			for i, name := range tt.repoNames {
				repoList[i] = repos.Repo{Name: name, Path: "/tmp/" + name}
			}
			r := repos.NewResolver(repoList)
			result := resolverForService(tt.serviceName, r)
			assert.Equal(t, tt.expectedRepo, result)
		})
	}
}

func TestResolverForService_NilResolver(t *testing.T) {
	result := resolverForService("auth-service", nil)
	assert.Equal(t, "", result)
}

func TestResolverForService_EmptyServiceName(t *testing.T) {
	r := repos.NewResolver([]repos.Repo{{Name: "auth-service", Path: "/tmp/auth-service"}})
	result := resolverForService("", r)
	assert.Equal(t, "", result)
}

// ── New constructor ──

func TestNew(t *testing.T) {
	c := New("http://localhost:11434", "qwen2.5-coder:3b")
	require.NotNil(t, c)
	assert.Equal(t, "http://localhost:11434", c.ollamaURL)
	assert.Equal(t, "qwen2.5-coder:3b", c.model)
	require.NotNil(t, c.cache)
}

// ── ClassificationResult ──

func TestClassificationResultFields(t *testing.T) {
	r := ClassificationResult{
		Protocol:      "http",
		TargetService: "auth-service",
		Endpoint:      "/api/v1/login",
		Confidence:    0.95,
		Reason:        "HTTP POST to /api/v1/login",
	}

	assert.Equal(t, "http", r.Protocol)
	assert.Equal(t, "auth-service", r.TargetService)
	assert.Equal(t, "/api/v1/login", r.Endpoint)
	assert.InDelta(t, 0.95, r.Confidence, 0.001)
	assert.Equal(t, "HTTP POST to /api/v1/login", r.Reason)
}
