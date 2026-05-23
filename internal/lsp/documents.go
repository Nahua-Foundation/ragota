package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Файл управляет жизненным циклом открытых документов на стороне клиента:
// отправляет textDocument/didOpen|didChange и ожидает publishDiagnostics
// как сигнал готовности первичного анализа файла LSP-сервером.

// DidOpen уведомляет LSP-сервер о содержимом файла.
// Отправляет didOpen только если файл ещё не открыт; если уже открыт и
// содержимое отличается — отправляет didChange (full-sync, упрощённо).
func (c *Client) DidOpen(path, languageID, text string) error {
	c.mu.Lock()
	old, exists := c.openedFiles[path]
	if exists && old == text {
		c.mu.Unlock()
		return nil
	}
	c.openedFiles[path] = text
	c.openedVers[path]++
	ver := c.openedVers[path]
	c.mu.Unlock()

	remotePath := c.toRemotePath(path)
	remoteURI := FileURI(remotePath)

	// Debug: логируем маппинг путей для диагностики "no views"
	lspDebug("LSP %s: DidOpen: local=%q remote=%q uri=%q rootURI=%q ver=%d\n",
		c.Language, path, remotePath, remoteURI, c.rootURI, ver)

	method := "textDocument/didOpen"
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        remoteURI,
			"languageId": languageID,
			"version":    ver,
			"text":       text,
		},
	}
	if exists {
		// Если уже был открыт, но контент изменился — используем didChange (упрощенно full sync)
		method = "textDocument/didChange"
		params = map[string]any{
			"textDocument": map[string]any{
				"uri":     remoteURI,
				"version": ver,
			},
			"contentChanges": []any{
				map[string]any{"text": text},
			},
		}
	}

	if err := c.Notify(method, params); err != nil {
		return err
	}
	// Уведомляем LSP сервер о новом файле в workspace — это заставляет gopls/jdtls
	// обновить internal views и увидеть файл для последующих hover/definition запросов.
	if method == "textDocument/didOpen" {
		_ = c.Notify("workspace/didChangeWatchedFiles", map[string]any{
			"changes": []any{
				map[string]any{"uri": remoteURI, "type": 1}, // 1 = Created
			},
		})
	}
	// Ждём publishDiagnostics как сигнал завершения первичного анализа файла.
	// Это критично для работы definition/hover/references — без ожидания
	// LSP сервер может вернуть null/пусто потому что ещё не проиндексировал файл.
	if method == "textDocument/didOpen" {
		// Небольшая задержка чтобы LSP сервер успел обработать didOpen
		time.Sleep(100 * time.Millisecond)
		// Разное время ожидания для разных языков:
		// - Python (pyright): быстрый анализ, 2-3 сек
		// - Go (gopls): средняя скорость, 3-4 сек
		// - Java (jdtls): медленная инициализация, 5-7 сек
		// - TypeScript: быстрый, 2-3 сек
		timeout := 3 * time.Second
		switch c.Language {
		case "java":
			timeout = 15 * time.Second // jdtls требует больше времени на индексацию
		case "go":
			timeout = 5 * time.Second // gopls нужно время на анализ workspace
		}
		c.waitForDiagnostics(path, timeout)
	}
	return nil
}

// waitForDiagnostics блокируется до получения publishDiagnostics с тем же
// путём, что и `path`, или до таймаута. При таймауте не возвращает ошибку —
// просто продолжает работу (вызывающая сторона может попробовать запрос).
func (c *Client) waitForDiagnostics(path string, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	started := time.Now()
	for {
		select {
		case uriPath := <-c.diagnosticsReady:
			if samePath(uriPath, path) {
				elapsed := time.Since(started)
				lspDebug("LSP %s: waitForDiagnostics DONE: path=%q elapsed=%v\n",
					c.Language, path, elapsed)
				return
			}
		case <-deadline.C:
			elapsed := time.Since(started)
			lspDebug("LSP %s: waitForDiagnostics TIMEOUT: path=%q after %v (no publishDiagnostics received)\n",
				c.Language, path, elapsed)
			return
		}
	}
}

// localFileLine читает указанную строку из файла на хосте.
// Используется только для эвристик сдвига позиции (см. Definition retry) —
// ошибки чтения молча игнорируются (вернётся пустая строка).
func (c *Client) localFileLine(path string, line int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line < 0 || line >= len(lines) {
		return ""
	}
	return lines[line]
}
