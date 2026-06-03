package lsp

import "context"

// Client — интерфейс LSP-клиента. Все операции принимают context для отмены.
// Реализация — session.Session.
type Client interface {
	// Definition выполняет textDocument/definition с fallback на declaration/typeDefinition.
	Definition(ctx context.Context, path string, line, character int) ([]Location, error)

	// References выполняет textDocument/references.
	References(ctx context.Context, path string, line, character int, includeDecl bool) ([]Location, error)

	// Hover выполняет textDocument/hover и возвращает текст подсказки.
	Hover(ctx context.Context, path string, line, character int) (string, error)

	// Implementation выполняет textDocument/implementation.
	Implementation(ctx context.Context, path string, line, character int) ([]Location, error)

	// DidOpen уведомляет сервер о содержимом файла (textDocument/didOpen или didChange).
	// Ожидает publishDiagnostics как сигнал готовности первичного анализа.
	DidOpen(ctx context.Context, path, languageID, text string) error

	// EnsureOpen открывает файл если он ещё не открыт. Идентично DidOpen для новых файлов.
	EnsureOpen(ctx context.Context, path string) error

	// Close закрывает соединение с LSP-сервером.
	Close() error

	// IsAlive возвращает true если процесс сервера жив.
	IsAlive() bool

	// Language возвращает язык клиента ("go", "java", "python", "typescript").
	Language() string
}
