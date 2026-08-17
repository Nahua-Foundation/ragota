package main

import (
	"fmt"
)

func main() {
	result := Add(10, 20)
	fmt.Printf("10 + 20 = %d\n", result)

	result2 := Multiply(5, 3)
	fmt.Printf("5 * 3 = %d\n", result2)

	area := CalculateRectangleArea(5.0, 3.0)
	fmt.Printf("Rectangle area (5x3): %.2f\n", area)

	greet := Greet("World")
	fmt.Println(greet)
}
