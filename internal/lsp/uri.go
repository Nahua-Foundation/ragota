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
// Сейчас сервер запускается локально, поэтому маппинг тождественный — функция
// оставлена точкой расширения (например, для docker-проброса).
func (c *Client) toRemotePath(localPath string) string {
	return localPath
}

// toLocalPath преобразует URI/путь, пришедший от сервера, обратно в локальный
// файловый путь хоста.
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
