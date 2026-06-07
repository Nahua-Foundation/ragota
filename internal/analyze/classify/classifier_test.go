package classify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifier_TestFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		path string
	}{
		{"internal/user/user_test.go"},
		{"src/components/Button.test.tsx"},
		{"tests/test_api.py"},
		{"src/__tests__/utils.spec.js"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := c.Classify(tt.path, "", nil, nil)
			assert.Equal(t, CategoryTest, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 90)
		})
	}
}

func TestClassifier_ConfigFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		path string
	}{
		{"config.yaml"},
		{"config/database.yml"},
		{"settings.toml"},
		{".env"},
		{"conf/app.conf"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := c.Classify(tt.path, "", nil, nil)
			assert.Equal(t, CategoryConfig, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 85)
		})
	}
}

func TestClassifier_DocumentationFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		path string
	}{
		{"README.md"},
		{"CHANGELOG.md"},
		{"docs/api.md"},
		{"documentation/guide.md"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := c.Classify(tt.path, "", nil, nil)
			assert.Equal(t, CategoryDocumentation, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 90)
		})
	}
}

func TestClassifier_InterfaceFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		name    string
		path    string
		content string
		imports []string
		sigs    []string
	}{
		{
			name:    "HTTP handler by path",
			path:    "internal/handler/user.go",
			content: "package handler",
			sigs:    []string{"func HandleGetUser(w http.ResponseWriter, r *http.Request)"},
		},
		{
			name:    "gRPC service",
			path:    "internal/grpc/user_service.go",
			content: "package grpc\n\nfunc (s *Server) GetUser(ctx context.Context, req *GetUserRequest) (*GetUserResponse, error)",
			imports: []string{"google.golang.org/grpc"},
			sigs:    []string{"func (s *Server) GetUser(ctx context.Context, req *GetUserRequest)"},
		},
		{
			name:    "REST controller",
			path:    "src/controllers/UserController.java",
			content: "@RestController\npublic class UserController",
			sigs:    []string{"@GetMapping(\"/users/{id}\")", "public User getUser(@PathVariable Long id)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.Classify(tt.path, tt.content, tt.imports, tt.sigs)
			assert.Equal(t, CategoryInterface, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 80)
		})
	}
}

func TestClassifier_InfrastructureFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		name    string
		path    string
		content string
		imports []string
	}{
		{
			name:    "Database repository",
			path:    "internal/repository/user_repo.go",
			content: "package repository",
			imports: []string{"database/sql", "github.com/lib/pq"},
		},
		{
			name:    "Redis client",
			path:    "internal/cache/redis.go",
			content: "package cache\n\nfunc NewRedisClient()",
			imports: []string{"github.com/go-redis/redis/v8"},
		},
		{
			name:    "External API client",
			path:    "internal/client/payment_gateway.go",
			content: "package client",
			imports: []string{"net/http"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.Classify(tt.path, tt.content, tt.imports, nil)
			assert.Equal(t, CategoryInfrastructure, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 75)
		})
	}
}

func TestClassifier_ModelFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		name    string
		path    string
		content string
		sigs    []string
	}{
		{
			name:    "Go model",
			path:    "internal/model/user.go",
			content: "package model",
			sigs: []string{
				"type User struct {",
				"type Address struct {",
				"type Email string",
			},
		},
		{
			name:    "TypeScript types",
			path:    "src/types/user.ts",
			content: "export interface User",
			sigs: []string{
				"interface User {",
				"interface Address {",
				"type UserID = string",
			},
		},
		{
			name:    "Python dataclass",
			path:    "src/entities/user.py",
			content: "from dataclasses import dataclass",
			sigs: []string{
				"@dataclass",
				"class User:",
				"class Address:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.Classify(tt.path, tt.content, nil, tt.sigs)
			assert.Equal(t, CategoryModel, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 70)
		})
	}
}

func TestClassifier_LogicFiles(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		name    string
		path    string
		content string
		sigs    []string
	}{
		{
			name: "Service with business logic",
			path: "internal/service/user_service.go",
			content: `package service

func (s *UserService) CreateUser(ctx context.Context, req CreateUserRequest) error {
	if err := s.validate(req); err != nil {
		return err
	}
	return s.repo.Save(ctx, req.ToUser())
}`,
			sigs: []string{
				"func (s *UserService) CreateUser(ctx context.Context, req CreateUserRequest) error",
				"func (s *UserService) validate(req CreateUserRequest) error",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.Classify(tt.path, tt.content, nil, tt.sigs)
			assert.Equal(t, CategoryLogic, result.Category)
			assert.GreaterOrEqual(t, result.Confidence, 65)
		})
	}
}

func TestClassifier_UnknownFiles(t *testing.T) {
	c := NewClassifier()

	// File with minimal content should be classified as unknown
	result := c.Classify("src/utils/misc.go", "package utils", nil, nil)
	assert.Equal(t, CategoryUnknown, result.Category)
	assert.Less(t, result.Confidence, 60)
}

func TestClassifier_Priority(t *testing.T) {
	c := NewClassifier()

	// Test file should be classified as test even if it has handler patterns
	result := c.Classify(
		"internal/handler/user_test.go",
		"package handler\n\nfunc TestGetUser(t *testing.T) {",
		[]string{"net/http"},
		[]string{"func TestGetUser(t *testing.T) {"},
	)
	assert.Equal(t, CategoryTest, result.Category, "test path should take priority")
}
