package repos

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// DetectLanguage returns the language for a file path based on its extension.
// The extension-to-language table lives in config, shared with the AST parser
// registry, so there is one list to keep in sync.
func DetectLanguage(path string) string {
	return config.LanguageForFile(path)
}

// GenerateID generates a unique repository ID from name and path/URL.
func GenerateID(name, pathOrURL string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + pathOrURL))
	return name + "-" + hex.EncodeToString(sum[:])[:12]
}

// HashContent returns the hex-encoded SHA-256 of the given content.
// Used to detect whether a file changed since it was last indexed.
func HashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// TestPathMarkers returns the markers IsTestPath matches, as a copy.
func TestPathMarkers() []string { return domain.TestPathMarkers() }

// GeneratedPathMarkers returns the markers IsGeneratedPath matches, as a copy.
func GeneratedPathMarkers() []string { return domain.GeneratedPathMarkers() }

// IsTestPath reports whether a repo-relative path looks like test, mock,
// fixture or vendored code rather than the code a question is about. See
// domain.IsTestPath.
func IsTestPath(path string) bool { return domain.IsTestPath(path) }

// IsGeneratedPath reports whether a repo-relative path holds generated code or
// the interface definition it was generated from. See domain.IsGeneratedPath.
func IsGeneratedPath(path string) bool { return domain.IsGeneratedPath(path) }
