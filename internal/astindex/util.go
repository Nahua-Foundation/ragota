package astindex

// Файл содержит мелкие чистые функции-помощники: определение языка по
// расширению, извлечение «имени» из ast.Expr, форматирование сигнатур
// и комментариев, хэш контента. Не имеют состояния и не зависят от Indexer.

import (
	"crypto/sha1"
	"encoding/hex"
	"go/ast"
	"path/filepath"
	"strings"
)

// detectLang возвращает код языка по расширению файла или пустую строку,
// если язык не поддерживается.
func detectLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
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
	}
	return ""
}

// exprName выводит «имя» из ast.Expr (идентификатор, селектор, указатель).
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		base := exprName(v.X)
		if base == "" {
			return v.Sel.Name
		}
		return base + "." + v.Sel.Name
	case *ast.StarExpr:
		return exprName(v.X)
	case *ast.IndexExpr:
		return exprName(v.X)
	case *ast.IndexListExpr:
		return exprName(v.X)
	}
	return ""
}

// signatureOf возвращает первую строку объявления функции (без тела),
// ограниченную 200 символами.
func signatureOf(src []byte, d *ast.FuncDecl) string {
	start := int(d.Pos() - 1)
	if start < 0 {
		start = 0
	}
	end := start
	if d.Body != nil {
		end = int(d.Body.Pos() - 1)
	} else {
		end = int(d.End() - 1)
	}
	if end > len(src) {
		end = len(src)
	}
	if start >= end {
		return ""
	}
	s := strings.TrimSpace(string(src[start:end]))
	if nl := strings.Index(s, "\n"); nl >= 0 {
		s = s[:nl]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// firstLine возвращает первую строку s, обрезанную до 200 символов.
func firstLine(s string) string {
	if nl := strings.Index(s, "\n"); nl >= 0 {
		s = s[:nl]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// commentText возвращает текст doc-комментария или пустую строку.
func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.TrimSpace(g.Text())
}

// hashBytes возвращает SHA-1 в hex-представлении.
func hashBytes(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
