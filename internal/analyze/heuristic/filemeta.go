package heuristic

import (
	"os"
	"path/filepath"
	"strings"

	"ragota/internal/analyze/types"
)

// EstimateLines оценивает количество строк по размеру файла.
func EstimateLines(size int64) int {
	if size <= 0 {
		return 0
	}
	return int(size / 40)
}

// BuildFileMeta собирает метаданные для одного файла.
func BuildFileMeta(path string) types.FileMeta {
	ext := strings.ToLower(filepath.Ext(path))
	info, err := os.Stat(path)
	if err != nil {
		return types.FileMeta{Path: path, Extension: ext}
	}

	head := ReadFileHead(path, 50)
	lines := strings.Split(head, "\n")

	var flags []string
	base := filepath.Base(path)
	if HasGeneratedMarker(head) {
		flags = append(flags, "generated")
	}
	if HasSpecMarker(head) {
		flags = append(flags, "spec")
	}
	if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "_test.ts") ||
		strings.HasSuffix(path, "_test.js") || strings.HasSuffix(path, "_test.py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		flags = append(flags, "test")
	}

	imports := ExtractImports(path, ext, lines)
	signatures := ExtractSignatures(path, ext, lines)
	chunks := ExtractSampleChunks(path, ext, lines)

	// Конвертируем старые string-чанки в новый формат types.Chunk
	sampleChunks := convertToChunks(chunks)

	return types.FileMeta{
		Path:         path,
		Extension:    ext,
		Size:         info.Size(),
		EstLines:     EstimateLines(info.Size()),
		Flags:        flags,
		Imports:      imports,
		Signatures:   signatures,
		SampleChunks: sampleChunks,
	}
}

// convertToChunks конвертирует []string в []types.Chunk.
func convertToChunks(chunkStrings []string) []types.Chunk {
	var chunks []types.Chunk
	for i, cs := range chunkStrings {
		position := "start"
		if i == 1 {
			position = "middle"
		} else if i >= 2 {
			position = "end"
		}
		chunks = append(chunks, types.Chunk{
			Content:   cs,
			StartLine: i*10 + 1,
			EndLine:   (i + 1) * 10,
			Position:  position,
		})
	}
	return chunks
}

// ExtractImports извлекает первые импорты из файла.
func ExtractImports(path, ext string, lines []string) []string {
	var imports []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch ext {
		case ".go":
			if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, `"`) {
				if strings.Contains(trimmed, `"`) {
					parts := strings.Split(trimmed, `"`)
					if len(parts) >= 2 {
						imports = append(imports, parts[1])
					}
				}
			}
		case ".ts", ".tsx", ".js", ".jsx", ".mjs":
			if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ") {
				if strings.Contains(trimmed, `"`) || strings.Contains(trimmed, `'`) {
					imports = append(imports, trimmed)
				}
			}
		case ".py":
			if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ") {
				imports = append(imports, trimmed)
			}
		case ".java":
			if strings.HasPrefix(trimmed, "import ") {
				imports = append(imports, trimmed)
			}
		}
		if len(imports) >= 10 {
			break
		}
	}
	return imports
}

// ExtractSignatures извлекает первые сигнатуры функций.
func ExtractSignatures(path, ext string, lines []string) []string {
	var signatures []string
	keywords := []string{"func ", "function ", "def ", "class ", "interface ", "type ", "export ", "public ", "private "}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, kw := range keywords {
			if strings.HasPrefix(trimmed, kw) || strings.Contains(trimmed, " "+kw) {
				sig := trimmed
				if len(sig) > 80 {
					sig = sig[:80] + "..."
				}
				signatures = append(signatures, sig)
				break
			}
		}
		if len(signatures) >= 5 {
			break
		}
	}
	return signatures
}

// ExtractSampleChunks извлекает репрезентативные чанки из файла.
func ExtractSampleChunks(path, ext string, lines []string) []string {
	switch ext {
	case ".go":
		return extractGoChunks(lines)
	case ".ts", ".tsx", ".js", ".jsx":
		return extractTsChunks(lines)
	case ".py":
		return extractPyChunks(lines)
	case ".java":
		return extractJavaChunks(lines)
	case ".json":
		return extractJsonChunks(lines)
	case ".yaml", ".yml":
		return extractYamlChunks(lines)
	case ".md":
		return extractMdChunks(lines)
	default:
		return firstLines(lines, 5)
	}
}

func extractGoChunks(lines []string) []string {
	firstSig := findFirstSignature(lines, []string{"func ", "type "})
	if firstSig >= 0 {
		return []string{joinLines(lines, firstSig, 3)}
	}
	return firstLines(lines, 3)
}

func extractTsChunks(lines []string) []string {
	firstSig := findFirstSignature(lines, []string{"export ", "function ", "class ", "const ", "interface "})
	if firstSig >= 0 {
		return []string{joinLines(lines, firstSig, 3)}
	}
	return firstLines(lines, 3)
}

func extractPyChunks(lines []string) []string {
	firstSig := findFirstSignature(lines, []string{"def ", "class "})
	if firstSig >= 0 {
		return []string{joinLines(lines, firstSig, 3)}
	}
	return firstLines(lines, 3)
}

func extractJavaChunks(lines []string) []string {
	firstSig := findFirstSignature(lines, []string{"public class ", "private class ", "public interface ", "void ", "public "})
	if firstSig >= 0 {
		return []string{joinLines(lines, firstSig, 3)}
	}
	return firstLines(lines, 3)
}

func extractJsonChunks(lines []string) []string {
	var keys []string
	for _, line := range lines[:min(50, len(lines))] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "\"") && strings.Contains(trimmed, "\":") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				if len(value) > 80 {
					value = value[:80] + "..."
				}
				keys = append(keys, key+":"+value)
			}
		}
		if len(keys) >= 3 {
			break
		}
	}

	var chunks []string
	if len(keys) > 0 {
		chunks = append(chunks, strings.Join(keys, "\n"))
	}

	content := strings.Join(lines, "\n")
	if strings.Contains(content, `"paths"`) || strings.Contains(content, `"openapi"`) {
		for i, line := range lines {
			if strings.Contains(line, `"/`) && strings.Contains(line, `":`) {
				chunks = append(chunks, joinLines(lines, i, 5))
				break
			}
		}
	}

	if len(chunks) == 0 {
		chunks = firstLines(lines, 5)
	}
	return chunks
}

func extractYamlChunks(lines []string) []string {
	var chunks []string

	var topLevel []string
	for _, line := range lines[:min(30, len(lines))] {
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.Contains(line, ":") {
			topLevel = append(topLevel, line)
		}
		if len(topLevel) >= 5 {
			break
		}
	}
	if len(topLevel) > 0 {
		chunks = append(chunks, strings.Join(topLevel, "\n"))
	}

	for i, line := range lines {
		if (strings.Contains(line, "services:") || strings.Contains(line, "paths:") ||
			strings.Contains(line, "definitions:") || strings.Contains(line, "environment:")) && i > 5 {
			chunks = append(chunks, joinLines(lines, i, 5))
			break
		}
	}

	if len(chunks) == 0 {
		chunks = firstLines(lines, 5)
	}
	return chunks
}

func extractMdChunks(lines []string) []string {
	var chunks []string

	var h1Found bool
	var para []string
	for _, line := range lines[:min(20, len(lines))] {
		if strings.HasPrefix(line, "# ") && !h1Found {
			chunks = append(chunks, line)
			h1Found = true
			continue
		}
		if h1Found && line != "" && !strings.HasPrefix(line, "#") {
			para = append(para, line)
			if len(para) >= 3 {
				break
			}
		}
	}
	if len(para) > 0 {
		chunks = append(chunks, strings.Join(para, "\n"))
	}

	for i, line := range lines {
		if strings.HasPrefix(line, "## ") && i > 10 {
			chunks = append(chunks, joinLines(lines, i, 5))
			break
		}
	}

	if len(chunks) == 0 {
		chunks = firstLines(lines, 5)
	}
	return chunks
}

func findFirstSignature(lines []string, patterns []string) int {
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		for _, p := range patterns {
			if strings.Contains(line, p) {
				return i
			}
		}
	}
	return -1
}

func joinLines(lines []string, start, n int) string {
	if start >= len(lines) {
		return ""
	}
	end := start + n
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

func firstLines(lines []string, n int) []string {
	if len(lines) < n {
		n = len(lines)
	}
	return []string{strings.Join(lines[:n], "\n")}
}
