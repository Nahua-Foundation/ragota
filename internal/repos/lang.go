package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"path/filepath"
)

// DetectLanguage returns the language for a file path based on its extension.
func DetectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".cs":
		return "csharp"
	case ".md":
		return "markdown"
	case ".rst":
		return "rst"
	case ".txt":
		return "text"
	case ".proto":
		return "proto"
	case ".sql":
		return "sql"
	case ".properties", ".env":
		return "properties"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	default:
		return ""
	}
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

// Markers naming the parts of a repository that are not the code a question is
// about. They are matched against MarkerPath rather than the raw path, so a
// directory at the repository root and the same directory nested below one read
// the same way: "/test/" covers both "test/x.go" and "src/test/x.go".
//
// The lists are exported through TestPathMarkers and GeneratedPathMarkers
// because the judgment also has to be made where Go cannot run — the SQL that
// ranks symbol lookups builds its penalty from them — and a second list written
// by hand in SQL would drift from this one.
var (
	testMarkers = []string{
		"/test/", "/tests/", "/testing/", "/testdata/", "/__tests__/",
		"/mocks/", "/mock/", "/fixtures/", "/vendor/", "/node_modules/",
		"_test.", ".test.", ".spec.",
	}

	// Every generated file in the benchmark corpus is protobuf/gRPC output:
	// genproto/*.pb.go, *_grpc.pb.go and *_pb2*.py. The C++ and JavaScript
	// spellings are the same generator's output for languages the corpus also
	// holds. ".proto" is listed with them on purpose: it is the declaration
	// those stubs are generated from, and never the code that implements the
	// behaviour a question asks about.
	generatedMarkers = []string{
		"/genproto/", "/generated/",
		".pb.go", ".pb.cc", ".pb.h",
		"_pb2.", "_pb2_", "_pb.js", "_pb.d.ts",
		".proto",
	}
)

// MarkerPath returns path in the form the markers are matched against:
// lower-cased and leading-slash anchored.
func MarkerPath(path string) string { return "/" + strings.ToLower(path) }

// TestPathMarkers returns the markers IsTestPath matches, as a copy.
func TestPathMarkers() []string { return slices.Clone(testMarkers) }

// GeneratedPathMarkers returns the markers IsGeneratedPath matches, as a copy.
func GeneratedPathMarkers() []string { return slices.Clone(generatedMarkers) }

// IsTestPath reports whether a repo-relative path looks like test, mock,
// fixture or vendored code rather than the code a question is about.
//
// Several rankings depend on this judgment — caller and contract promotion
// demote such paths, the symbol-summary pass skips them, and symbol lookups
// rank them last — so it is one function rather than one per feature: two
// features disagreeing about what a test file is would rank the same repository
// two different ways.
func IsTestPath(path string) bool { return matchesMarker(path, testMarkers) }

// IsGeneratedPath reports whether a repo-relative path holds generated code or
// the interface definition it was generated from.
//
// It is the sibling of IsTestPath and exists for the same reason: ten generated
// gRPC stubs named ShipOrder are not what a question about shipping an order is
// asking for, and every ranking that has to say so must say it the same way.
func IsGeneratedPath(path string) bool { return matchesMarker(path, generatedMarkers) }

func matchesMarker(path string, markers []string) bool {
	p := MarkerPath(path)
	for _, m := range markers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}
