// Package lsp реализует минимальный LSP-клиент (JSON-RPC 2.0 над stdio)
// и менеджер серверов на язык. Используется MCP-сервером.
//
// Реализация декомпозирована по доменам:
//
//   - client.go       — структура Client (тип-агрегатор);
//   - debug.go        — отладочный лог-файл;
//   - server_spec.go  — описание запускаемых LSP-серверов (DefaultServers);
//   - lifecycle.go    — Start/Close/initialize и сбор stderr/процессной диагностики;
//   - jsonrpc.go      — транспорт JSON-RPC 2.0: типы, Call/Notify, readLoop,
//     обработка серверных request/notification;
//   - uri.go          — конвертация file:// URI ↔ путь, samePath, isAlphaNum;
//   - types.go        — внутренние LSP-типы (Range/Position/Location/...) и
//     декодеры результатов textDocument/* + positionParams,
//     hoverString;
//   - documents.go    — DidOpen/waitForDiagnostics и чтение строки файла;
//   - navigation.go   — публичные операции Definition/References/Hover/Implementation;
//   - client_*.go     — per-language капабилити/настройки (Go/Java/Python/TS).
package lsp

import (
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Client — простой LSP-клиент над одним процессом.
//
// Безопасен для конкурентного использования: все операции с stdin/stdout/
// pending защищены `mu`; атомарные флаги (initialized/closed/dead/
// javaReadyClosed) — `atomic.Bool`. Жизненный цикл управляется через
// Start (см. lifecycle.go) и Close.
type Client struct {
	// Language — код языка ("go" | "typescript" | "python" | "java"),
	// используется для выбора per-language capabilities/config.
	Language string

	// Процесс и его пайпы.
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// Транспорт JSON-RPC.
	mu          sync.Mutex
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	initialized atomic.Bool

	// Корни и workspace.
	rootURI   string
	localRoot string

	// Маппинг путей host ↔ контейнер для docker-режима. Когда LSP запущен в
	// контейнере, пути в URI должны соответствовать тому, что видит процесс
	// внутри контейнера (например, /workspace), а не путям на хосте. Если
	// hostRoot пустой — режим тождественного маппинга (локальный запуск).
	hostRoot   string // абсолютный путь корня проекта на хосте
	remoteRoot string // путь корня внутри контейнера (например, /workspace)

	// Состояние открытых документов.
	openedFiles      map[string]string // path -> last sent content
	openedVers       map[string]int    // path -> version
	diagnosticsReady chan string       // сигнал publishDiagnostics: путь, для которого пришло

	// Диагностика процесса.
	stderrLines []string
	processErr  error
	processDone chan struct{}

	// javaReady закрывается, когда jdtls присылает notification
	// "language/status" с type "ServiceReady" или "Started". Это
	// jdtls-специфичное расширение LSP, означающее, что импорт проектов
	// и индексация завершены и сервер готов отвечать на definition/hover/
	// publishDiagnostics. До этого момента ответы будут пустыми, поэтому
	// initialize дожидается этого сигнала (см. initialize).
	javaReady       chan struct{}
	javaReadyClosed atomic.Bool

	// goplsReady закрывается, когда gopls завершает начальную индексацию.
	// Используется window/workDoneProgress с токеном "gopls.indexing".
	goplsReady       chan struct{}
	goplsReadyClosed atomic.Bool

	// Флаги завершения.
	closed atomic.Bool
	dead   atomic.Bool
}
