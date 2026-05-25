package fileutil

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Matcher решает, надо ли игнорировать путь.
// Каждый паттерн матчится либо по имени компонента (basename),
// либо как glob по относительному пути.
type Matcher struct {
	patterns []string
}

func NewMatcher(patterns []string) *Matcher {
	return &Matcher{patterns: slices.Clone(patterns)}
}

// IsIgnored возвращает true, если относительный путь rel должен быть пропущен.
// isDir указывает, является ли путь директорией (важно для оптимизации обхода).
func (m *Matcher) IsIgnored(rel string, isDir bool) bool {
	if rel == "." || rel == "" {
		return false
	}
	base := filepath.Base(rel)
	for _, p := range m.patterns {
		// прямое совпадение по basename — самый частый случай (vendor, node_modules)
		if p == base {
			return true
		}
		// glob по basename
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		// glob по относительному пути
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		// поддиректория ignored-каталога на любом уровне вложенности
		// Проверяем, содержится ли паттерн как компонент пути
		if strings.Contains(string(filepath.Separator)+rel+string(filepath.Separator), string(filepath.Separator)+p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// WalkFiles обходит root, отдавая абсолютные пути файлов с подходящими расширениями.
// Игнорирует пути, на которые срабатывает matcher.
// extensions — список с точкой (".go"); если nil/empty — берутся все файлы.
func WalkFiles(root string, m *Matcher, extensions []string, fn func(absPath, relPath string, info fs.FileInfo) error) error {
	extSet := make(map[string]struct{}, len(extensions))
	for _, e := range extensions {
		extSet[strings.ToLower(e)] = struct{}{}
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // не падаем на permission denied и т.п.
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if m != nil && m.IsIgnored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if m != nil && m.IsIgnored(rel, false) {
			return nil
		}
		if len(extSet) > 0 {
			if _, ok := extSet[strings.ToLower(filepath.Ext(path))]; !ok {
				return nil
			}
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		return fn(path, rel, info)
	})
}

// HashFile возвращает sha1 содержимого файла (hex).
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes возвращает sha1 от слайса байт (hex).
func HashBytes(data []byte) string {
	h := sha1.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// LanguageByExt возвращает каноническое имя языка по расширению, либо "".
func LanguageByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".java":
		return "java"
	case ".proto":
		return "proto"
	case ".md", ".rst", ".txt":
		return "text"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	}
	return ""
}

// SecureJoin безопасно соединяет root и rel, проверяя, что результат
// находится внутри root. Если rel абсолютный — проверяет его нахождение в root.
func SecureJoin(root, rel string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var absRes string
	if filepath.IsAbs(rel) {
		absRes = filepath.Clean(rel)
	} else {
		absRes = filepath.Join(absRoot, rel)
	}

	// Проверяем, что absRes начинается с absRoot + разделитель,
	// либо равен absRoot.
	rel2, err := filepath.Rel(absRoot, absRes)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel2, ".."+string(filepath.Separator)) || rel2 == ".." {
		return "", fmt.Errorf("path traversal detected: %s is outside of %s", rel, root)
	}
	return absRes, nil
}
