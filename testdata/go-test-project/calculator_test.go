package main

import (
	"testing"
)

func TestAdd(t *testing.T) {
	tests := []struct {
		name     string
		a, b     int
		expected int
	}{
		{"positive", 5, 3, 8},
		{"negative", -5, -3, -8},
		{"mixed", -5, 3, -2},
		{"zero", 0, 5, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Add(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("Add(%d, %d) = %d, expected %d", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestMultiply(t *testing.T) {
	tests := []struct {
		name     string
		a, b     int
		expected int
	}{
		{"positive", 5, 3, 15},
		{"negative", -5, 3, -15},
		{"zero", 5, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Multiply(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("Multiply(%d, %d) = %d, expected %d", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestDivide(t *testing.T) {
	t.Run("valid division", func(t *testing.T) {
		result, err := Divide(10, 2)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if result != 5 {
			t.Errorf("Divide(10, 2) = %d, expected 5", result)
		}
	})

	t.Run("division by zero", func(t *testing.T) {
		_, err := Divide(10, 0)
		if err == nil {
			t.Error("expected error for division by zero, got nil")
		}
	})
}

func TestPerson(t *testing.T) {
	p := NewPerson("Alice", 25)

	if p.Name != "Alice" {
		t.Errorf("expected name 'Alice', got '%s'", p.Name)
	}

	if p.Age != 25 {
		t.Errorf("expected age 25, got %d", p.Age)
	}

	if !p.IsAdult() {
		t.Error("expected person to be adult")
	}

	child := NewPerson("Bob", 10)
	if child.IsAdult() {
		t.Error("expected child not to be adult")
	}
}

func TestGreet(t *testing.T) {
	result := Greet("World")
	expected := "Hello, World!"
	if result != expected {
		t.Errorf("Greet('World') = '%s', expected '%s'", result, expected)
	}
}

// BenchmarkAdd benchmarks the Add function.
func BenchmarkAdd(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Add(100, 200)
	}
}
