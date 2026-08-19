package main

import "fmt"

// Add adds two numbers together.
func Add(a, b int) int {
	return a + b
}

// Subtract subtracts b from a.
func Subtract(a, b int) int {
	return a - b
}

// Multiply multiplies two numbers together.
func Multiply(a, b int) int {
	return a * b
}

// Divide divides a by b.
func Divide(a, b int) (int, error) {
	if b == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	return a / b, nil
}

// CalculateRectangleArea calculates the area of a rectangle.
func CalculateRectangleArea(width, height float64) float64 {
	return width * height
}

// CalculateCircleArea calculates the area of a circle.
func CalculateCircleArea(radius float64) float64 {
	return 3.14159 * radius * radius
}

// Greet returns a greeting message.
func Greet(name string) string {
	return "Hello, " + name + "!"
}

// Person represents a person with a name and age.
type Person struct {
	Name string
	Age  int
}

// NewPerson creates a new Person.
func NewPerson(name string, age int) *Person {
	return &Person{
		Name: name,
		Age:  age,
	}
}

// String returns the string representation of a Person.
func (p *Person) String() string {
	return fmt.Sprintf("%s (%d years old)", p.Name, p.Age)
}

// IsAdult returns true if the person is 18 or older.
func (p *Person) IsAdult() bool {
	return p.Age >= 18
}
