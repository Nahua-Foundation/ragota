// Package main — тонкая точка входа бинаря ragota.
// Вся логика CLI находится в internal/app/cli.
package main

import "ragota/internal/app/cli"

func main() {
	cli.Execute()
}
