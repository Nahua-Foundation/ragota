package lsp

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// Файл содержит хелперы для конвертации между локальными путями и file:// URI,
// а также утилиты сравнения путей. Используются всеми участками клиента,
// которые отдают/принимают URI в LSP-протоколе.

// FileURI возвращает file:// URI для абсолютного пути.
func FileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

// pathFromFileURI парсит file:// URI и возвращает локальный путь.
// Для Windows корректно отбрасывает ведущий слэш перед буквой диска.
func pathFromFileURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	if runtime.GOOS == "windows" && strings.HasPrefix(p, "/") && len(p) > 3 && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// toRemotePath преобразует локальный путь хоста в путь, видимый LSP-серверу.
// В docker-режиме хостовый префикс (hostRoot) заменяется на remoteRoot
// (например, /workspace) — именно по такому пути проект смонтирован
// в контейнере. В локальном режиме маппинг тождественный.
func (c *Client) toRemotePath(localPath string) string {
	if c == nil || c.hostRoot == "" || c.remoteRoot == "" {
		return localPath
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		abs = localPath
	}
	abs = filepath.Clean(abs)
	host := filepath.Clean(c.hostRoot)
	if abs == host {
		return c.remoteRoot
	}
	if rel, err := filepath.Rel(host, abs); err == nil && !strings.HasPrefix(rel, "..") {
		// Внутри контейнера пути — POSIX, поэтому собираем через "/".
		return c.remoteRoot + "/" + filepath.ToSlash(rel)
	}
	return localPath
}

// toLocalPath преобразует URI/путь, пришедший от сервера, обратно в локальный
// файловый путь хоста. В docker-режиме префикс remoteRoot заменяется на
// hostRoot, чтобы вызывающий код мог работать с обычными хостовыми путями.
func (c *Client) toLocalPath(remoteURI string) string {
	path := remoteURI
	if strings.HasPrefix(remoteURI, "file://") {
		u, err := url.Parse(remoteURI)
		if err == nil {
			path = u.Path
			if strings.HasPrefix(path, "/") && len(path) > 3 && path[2] == ':' {
				path = path[1:]
			}
		}
	}
	if c != nil && c.hostRoot != "" && c.remoteRoot != "" {
		clean := filepath.ToSlash(filepath.Clean(path))
		remote := filepath.ToSlash(filepath.Clean(c.remoteRoot))
		if clean == remote {
			return filepath.Clean(c.hostRoot)
		}
		if strings.HasPrefix(clean, remote+"/") {
			rel := strings.TrimPrefix(clean, remote+"/")
			return filepath.Join(c.hostRoot, filepath.FromSlash(rel))
		}
	}
	return filepath.FromSlash(path)
}

// samePath сравнивает два пути после Abs+Clean. Используется при ожидании
// publishDiagnostics — LSP-сервер может прислать слегка иначе нормализованный URI.
func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil {
		a = absA
	}
	if errB == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// isAlphaNum — быстрый ASCII-классификатор для эвристик поиска идентификаторов
// в строке исходника (см. Definition retry).
func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
