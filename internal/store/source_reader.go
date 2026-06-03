// Package store — SourceReader utility для чтения исходного кода по AST unit.

package store

import (
	"os"
	"strings"
)

// SourceOptions задаёт параметры чтения исходного кода.
type SourceOptions struct {
	// BeforeLines — сколько строк до определения включить.
	BeforeLines int
	// AfterLines — сколько строк после определения включить.
	AfterLines int
	// FullFile — если true, вернуть весь файл (игнорирует BeforeLines/AfterLines).
	FullFile bool
}

// ReadSource читает исходный код для AST unit.
// Возвращает (source, error). Если файл недоступен — ("", nil).
func (u *ASTUnit) ReadSource(opts SourceOptions) (string, error) {
	if u.FilePath == "" {
		return "", nil
	}
	src, err := os.ReadFile(u.FilePath)
	if err != nil {
		return "", err
	}
	if opts.FullFile {
		return string(src), nil
	}
	lines := strings.Split(string(src), "\n")
	start := u.StartLine - 1 - opts.BeforeLines
	end := u.EndLine + opts.AfterLines
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// ReadFullSource читает весь файл целиком.
func (u *ASTUnit) ReadFullSource() (string, error) {
	return u.ReadSource(SourceOptions{FullFile: true})
}
