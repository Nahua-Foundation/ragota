// Package lsp реализует минимальный LSP-клиент (JSON-RPC 2.0 над stdio)
// и менеджер серверов на язык. Используется MCP-сервером.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	lspDebugLog  *os.File
	lspDebugOnce sync.Once
)

// openLspDebugLog лениво открывает файл лога LSP в .ai-tools/logs/lsp-debug.log
// относительно текущего рабочего каталога (корня проекта, где запущен ai-tools).
// Папка создаётся при необходимости. Путь можно переопределить переменной
// окружения AI_TOOLS_LSP_LOG (абсолютный путь к файлу).
func openLspDebugLog() {
	lspDebugOnce.Do(func() {
		path := os.Getenv("AI_TOOLS_LSP_LOG")
		if path == "" {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			dir := filepath.Join(cwd, ".ai-tools", "logs")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return
			}
			path = filepath.Join(dir, "lsp-debug.log")
		} else {
			if d := filepath.Dir(path); d != "" {
				_ = os.MkdirAll(d, 0o755)
			}
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			lspDebugLog = f
		}
	})
}

func lspDebug(format string, args ...any) {
	openLspDebugLog()
	if lspDebugLog != nil {
		fmt.Fprintf(lspDebugLog, format, args...)
	}
}

// julHeaderRe матчит строку-заголовок java.util.logging вида:
//
//	"May 23, 2026 3:09:54 PM org.apache.aries.spifly.BaseActivator log"
//
// За такой строкой обычно идёт вторая с уровнем (INFO:/WARNING:/...).
var julHeaderRe = regexp.MustCompile(`^[A-Z][a-z]+ \d{1,2}, \d{4} \d{1,2}:\d{2}:\d{2} (AM|PM) \S+ (log|logp|logrb|info|warning|fine|finer|finest|config|severe)$`)

// ServerSpec описывает запускаемый LSP-сервер.
type ServerSpec struct {
	Language  string   // "go" | "typescript" | "python" | "java"
	Command   string   // например "gopls"
	Args      []string // аргументы
	LocalRoot string   // локальный корень проекта
}

// DefaultServers — рекомендуемые LSP-серверы.
// Если бинаря нет в PATH, сервер для этого языка просто не стартует.
func DefaultServers() []ServerSpec {
	return []ServerSpec{
		{Language: "go", Command: "gopls"},
		{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
		{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
		{Language: "java", Command: "jdtls", Args: []string{
			"--jvm-arg", "-Xmx4G",
			"--jvm-arg", "--add-opens=java.base/sun.misc=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=java.base/java.lang.reflect=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=java.base/java.util=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.api=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.util=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.code=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.main=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.tree=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.model=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.comp=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.file=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.jvm=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.parser=ALL-UNNAMED",
			"--jvm-arg", "--add-opens=jdk.compiler/com.sun.tools.javac.processing=ALL-UNNAMED",
			"-data", ".ai-tools/jdtls-data",
		}},
	}
}

// Client — простой LSP-клиент над одним процессом.
type Client struct {
	Language string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser

	mu          sync.Mutex
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	initialized atomic.Bool

	rootURI   string
	localRoot string

	openedFiles      map[string]string // path -> last sent content
	openedVers       map[string]int    // path -> version
	diagnosticsReady chan string
	stderrLines      []string
	processErr       error
	processDone      chan struct{}

	// javaReady закрывается, когда jdtls присылает notification
	// "language/status" с type "ServiceReady" или "Started". Это
	// jdtls-специфичное расширение LSP, означающее, что импорт проектов
	// и индексация завершены и сервер готов отвечать на definition/hover/
	// publishDiagnostics. До этого момента ответы будут пустыми, поэтому
	// initialize дожидается этого сигнала (см. initialize).
	javaReady       chan struct{}
	javaReadyClosed atomic.Bool

	closed atomic.Bool
	dead   atomic.Bool
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Err     error           `json:"-"`
}

// rpcIncoming используется для разбора входящих сообщений от сервера,
// которые могут быть либо ответами (есть result/error), либо запросами
// от сервера к клиенту (есть method и id), либо уведомлениями (method без id).
type rpcIncoming struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// rpcServerResponse — ответ клиента на серверный request.
type rpcServerResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Start запускает процесс LSP-сервера и выполняет initialize/initialized.
func Start(ctx context.Context, spec ServerSpec, root string) (*Client, error) {
	// Обрабатываем относительные пути в аргументах (например, -data .ai-tools/jdtls-data)
	// так как рабочий каталог процесса совпадает с рабочим каталогом AI-tools,
	// а не с root проекта, если они разные.
	// Мы хотим, чтобы -data создавался внутри проекта:
	args := make([]string, len(spec.Args))
	copy(args, spec.Args)
	for i, arg := range args {
		if (arg == "-data" || arg == "--data") && i+1 < len(args) {
			dataDir := args[i+1]
			if !filepath.IsAbs(dataDir) && !strings.HasPrefix(dataDir, "/") {
				args[i+1] = filepath.Join(root, dataDir)
			}
		}
	}

	// ВАЖНО: НЕ используем exec.CommandContext(ctx, ...) — иначе при отмене
	// ctx (а он приходит из RPC-обработчика lspsrv.go с `defer cancel()`
	// после каждого Hover/Definition/References) Go убивает процесс SIGKILL'ом
	// сразу после ответа. Это приводило к симптому: успешный ответ → mgnt
	// мгновенно "client dead, recreating" → следующий запрос пересоздаёт jdtls
	// с нуля (импорт 5-10с). Жизненный цикл процесса должен управляться только
	// Manager.Close() (Ctrl+C / выход приложения), а не таймаутом одного запроса.
	_ = ctx
	cmd := exec.Command(spec.Command, args...)
	cmd.Dir = root // Устанавливаем рабочий каталог в корень проекта

	// Создаем директорию для данных, если она указана в аргументах и еще не существует
	for i, arg := range args {
		if (arg == "-data" || arg == "--data") && i+1 < len(args) {
			_ = os.MkdirAll(args[i+1], 0755)
		}
		// Обрабатываем аргументы вида --jvm-arg=-Dfoo=bar или --jvm-arg=--add-opens=...
		if strings.HasPrefix(arg, "--jvm-arg=") || strings.HasPrefix(arg, "-jvm-arg=") {
			// Пропускаем, это JVM аргументы
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Command, err)
	}

	rootURI := FileURI(root)
	localRoot := root

	c := &Client{
		Language:         spec.Language,
		cmd:              cmd,
		stdin:            stdin,
		stdout:           stdout,
		stderr:           stderr,
		pending:          make(map[int64]chan rpcResponse),
		rootURI:          rootURI,
		localRoot:        localRoot,
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.processErr = err
		c.mu.Unlock()
		close(c.processDone)
		// Логируем детали выхода процесса — критично для диагностики крашей
		// jdtls (SIGKILL от OOM-killer, ненулевой exit code и т.п.).
		details := c.processSummary()
		tail := c.stderrSummary()
		lspDebug("LSP %s: process exited: %s; stderr tail: %s\n",
			c.Language, details, tail)
	}()
	go c.readLoop()
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			// Фильтруем «шумные» не-ошибочные строки jdtls/pyright/tsserver:
			// java.util.logging уровней INFO/FINE/CONFIG, SLF4J-регистрации,
			// предупреждения JVM про incubator modules и т.п.
			// ВАЖНО: фильтр применяется и к rememberStderr — иначе реальные
			// ошибки jdtls (Exception/SEVERE/!ENTRY) вытесняются десятками
			// WARNING'ов JVM из буфера и в recent stderr мы видим только шум.
			lower := strings.ToLower(trimmed)
			noisy := strings.HasPrefix(trimmed, "WARNING:") ||
				strings.Contains(trimmed, "INFO:") ||
				strings.Contains(trimmed, "FINE:") ||
				strings.Contains(trimmed, "FINER:") ||
				strings.Contains(trimmed, "FINEST:") ||
				strings.Contains(trimmed, "CONFIG:") ||
				strings.Contains(lower, "using incubator modules") ||
				strings.Contains(lower, "registered provider") ||
				strings.Contains(lower, "unsafe") ||
				strings.Contains(lower, "final field mutation") ||
				strings.Contains(lower, "reflectively by class") ||
				strings.Contains(lower, "to avoid a warning") ||
				strings.Contains(lower, "will be blocked in a future release") ||
				strings.Contains(lower, "illegal reflective access") ||
				strings.Contains(lower, "please consider reporting this to the maintainers") ||
				strings.Contains(lower, "use --enable-final-field-mutation") ||
				strings.Contains(lower, "org.eclipse.sisu.inject") ||
				strings.Contains(lower, "guice-5.1.0-classes.jar") ||
				strings.HasPrefix(trimmed, "SLF4J:") ||
				julHeaderRe.MatchString(trimmed)
			if !noisy {
				c.rememberStderr(trimmed)
			}
			if noisy {
				continue
			}
			// Печатаем оставшиеся (предположительно реальные ошибки/предупреждения) в stderr для отладки.
			if spec.Language == "java" {
				lspDebug("LSP %s stderr: %s\n", spec.Language, line)
			} else {
				_, _ = fmt.Fprintf(os.Stderr, "LSP %s stderr: %s\n", spec.Language, line)
			}
		}
	}()

	if err := c.initialize(ctx, root); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s: %w", spec.Language, err)
	}
	return c, nil
}

// IsAlive возвращает true, если процесс сервера запущен и readLoop работает.
func (c *Client) IsAlive() bool {
	if c.closed.Load() || c.dead.Load() {
		lspDebug("LSP %s: IsAlive=false (closed=%v dead=%v)\n", c.Language, c.closed.Load(), c.dead.Load())
		return false
	}
	// Дополнительная проверка: жив ли процесс на самом деле
	if c.cmd != nil && c.cmd.Process != nil {
		// Проверяем, существует ли процесс (kill -0)
		if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
			lspDebug("LSP %s: IsAlive=false (process dead: %v)\n", c.Language, err)
			c.dead.Store(true)
			return false
		}
	}
	lspDebug("LSP %s: IsAlive=true\n", c.Language)
	return true
}

// Close завершает процесс.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.processDone
	return c.processErr
}

func (c *Client) initialize(ctx context.Context, root string) error {
	// processId должен быть PID родительского процесса, который сервер мониторит,
	// чтобы завершиться при его смерти (через kill(pid, 0)).
	// Отключаем мониторинг для всех LSP, чтобы они не закрывались при разрыве
	// соединения с MCP клиентом (SSE/stdio).
	var processID any = nil
	params := map[string]any{
		"processId":    processID,
		"rootPath":     pathFromFileURI(c.rootURI),
		"rootUri":      c.rootURI,
		"capabilities": c.capabilities(),
		"workspaceFolders": []map[string]any{
			{"uri": c.rootURI, "name": "root"},
		},
	}
	// initializationOptions/settings — некоторые серверы (pyright) читают
	// настройки уже на этапе initialize, не дожидаясь workspace/configuration.
	if initOpts := c.initializationOptions(); initOpts != nil {
		params["initializationOptions"] = initOpts
	}
	var res json.RawMessage
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	// Явно отправляем didChangeConfiguration — это заставляет pyright/tsserver
	// перечитать настройки через workspace/configuration сразу после initialized,
	// не дожидаясь первого textDocument/* запроса.
	if settings := c.configFor(""); settings != nil {
		_ = c.Notify("workspace/didChangeConfiguration", map[string]any{
			"settings": settings,
		})
	}
	// gopls требует явного уведомления workspace/didChangeWorkspaceFolders
	// после инициализации для корректной индексации файлов.
	if c.Language == "go" {
		_ = c.Notify("workspace/didChangeWorkspaceFolders", map[string]any{
			"event": "created",
			"workspaceFolders": []map[string]any{
				{"uri": c.rootURI, "name": "workspace"},
			},
		})
	}
	if c.Language == "java" {
		// jdtls часто требует уведомления о пустом classpath или аналогичном событии,
		// но главное — убедиться, что он проиндексировал корень.
		_ = c.Notify("workspace/didChangeConfiguration", map[string]any{
			"settings": map[string]any{
				"java": javaConfigFor("java"),
			},
		})
		// jdtls (в отличие от gopls/pyright/tsserver) после initialized НЕ готов
		// сразу: ему нужно поднять OSGi-контейнер, импортировать Maven/Gradle
		// проекты, скачать зависимости и построить classpath. До этого момента
		// definition/hover возвращают пустые результаты, а publishDiagnostics
		// вообще не приходит. Готовность сигнализируется нестандартным
		// notification "language/status" с type:"ServiceReady" (или "Started"
		// для invisible-project режима) — он обрабатывается в
		// handleServerNotification и закрывает канал c.javaReady.
		//
		// Таймаут можно переопределить через JDTLS_READY_TIMEOUT (в секундах),
		// по умолчанию 120с — достаточно для среднего Maven-проекта на первом
		// запуске. При таймауте не падаем, а просто продолжаем: даже частично
		// готовый jdtls лучше, чем ошибка инициализации.
		readyTimeout := 120 * time.Second
		if v := os.Getenv("JDTLS_READY_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				readyTimeout = time.Duration(n) * time.Second
			}
		}
		select {
		case <-c.javaReady:
			lspDebug("LSP java: ready signal received\n")
		case <-c.processDone:
			return fmt.Errorf("jdtls process exited before becoming ready: %v",
				c.processErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyTimeout):
			lspDebug("LSP java: ready timeout after %s, continuing anyway\n",
				readyTimeout)
		}
	}
	c.initialized.Store(true)
	return nil
}

// initializationOptions возвращает initializationOptions для конкретного
// LSP-сервера. Pyright использует их для немедленной настройки анализа
// (без них python.analysis.* применяются с задержкой или вообще игнорируются
// для проектов без pyrightconfig.json).
func (c *Client) initializationOptions() map[string]any {
	switch c.Language {
	case "go":
		return goInitializationOptions()
	case "java":
		return javaInitializationOptions()
	case "python":
		return pythonInitializationOptions()
	case "typescript", "javascript":
		return typescriptInitializationOptions()
	}
	return nil
}

// capabilities возвращает capabilities для initialize запроса.
// Разные LSP-серверы имеют разные требования к формату.
func (c *Client) capabilities() map[string]any {
	switch c.Language {
	case "go":
		return goCapabilities()
	case "java":
		return javaCapabilities()
	case "python":
		return pythonCapabilities()
	case "typescript", "javascript":
		return typescriptCapabilities()
	}
	// Default capabilities
	return map[string]any{
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"didOpen":             true,
				"didChange":           true,
				"didSave":             true,
			},
			"definition": map[string]any{
				"linkSupport": true,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"configuration":    true,
		},
	}
}

// Call отправляет запрос и ждёт ответ.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Err != nil {
			return resp.Err
		}
		if resp.Error != nil {
			return fmt.Errorf("lsp %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-time.After(120 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("lsp %s timeout", method)
	}
}

// Notify шлёт уведомление без ID.
func (c *Client) Notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return c.withStderrLocked(err)
	}
	if _, err := c.stdin.Write(data); err != nil {
		return c.withStderrLocked(err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.failPending(fmt.Errorf("lsp %s process stopped before responding", c.Language))
	defer c.dead.Store(true)
	br := bufio.NewReader(c.stdout)
	for {
		length := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("Content-Length:"):])
				n, err := strconv.Atoi(v)
				if err == nil {
					length = n
				}
			}
		}
		if length < 0 {
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		// lspDebug("LSP %s incoming: %s\n", c.Language, string(buf))
		var msg rpcIncoming
		if err := json.Unmarshal(buf, &msg); err != nil {
			continue
		}
		// Случай 1: серверный request (method != "" && id != nil) — обязаны ответить.
		// ВАЖНО: обработка идёт в отдельной горутине, иначе readLoop блокируется
		// на writeServerResponse → берёт c.mu (которая может быть удержана Call'ом),
		// и pyright/tsserver встают по таймауту, не успев ответить на initialize.
		if msg.Method != "" && msg.ID != nil {
			id := *msg.ID
			method := msg.Method
			params := msg.Params
			go c.handleServerRequest(id, method, params)
			continue
		}
		// Случай 2: серверное уведомление (method != "" && id == nil).
		// Pyright присылает publishDiagnostics после первичного анализа didOpen;
		// используем это как сигнал, что definition уже можно спрашивать без гонки.
		if msg.Method != "" {
			switch msg.Method {
			case "textDocument/publishDiagnostics", "language/status":
				c.handleServerNotification(msg.Method, msg.Params)
			}
			continue
		}
		// Случай 3: ответ на наш запрос.
		if msg.ID == nil {
			continue
		}
		var id int64
		if err := json.Unmarshal(*msg.ID, &id); err != nil || id == 0 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ok {
			ch <- rpcResponse{JSONRPC: msg.JSONRPC, ID: id, Result: msg.Result, Error: msg.Error}
		}
	}
}

func (c *Client) failPending(err error) {
	if processDetails := c.processSummary(); processDetails != "" {
		err = fmt.Errorf("%w; process: %s", err, processDetails)
	}
	if details := c.stderrSummary(); details != "" {
		err = fmt.Errorf("%w; recent stderr: %s", err, details)
	}
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.mu.Unlock()
	for id, ch := range pending {
		ch <- rpcResponse{ID: id, Err: err}
	}
}

func (c *Client) rememberStderr(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stderrLines = append(c.stderrLines, line)
	if len(c.stderrLines) > 200 {
		c.stderrLines = c.stderrLines[len(c.stderrLines)-200:]
	}
}

func (c *Client) stderrSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.stderrLines, " | ")
}

func (c *Client) withStderr(err error) error {
	if err == nil {
		return nil
	}
	if processDetails := c.processSummary(); processDetails != "" {
		err = fmt.Errorf("%w; process: %s", err, processDetails)
	}
	if details := c.stderrSummary(); details != "" {
		return fmt.Errorf("%w; recent stderr: %s", err, details)
	}
	return err
}

func (c *Client) withStderrLocked(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-c.processDone:
		if c.processErr != nil {
			err = fmt.Errorf("%w; process exited: %v", err, c.processErr)
		} else {
			err = fmt.Errorf("%w; process exited successfully", err)
		}
	default:
	}
	if len(c.stderrLines) > 0 {
		return fmt.Errorf("%w; recent stderr: %s", err, strings.Join(c.stderrLines, " | "))
	}
	return err
}

func (c *Client) processSummary() string {
	select {
	case <-c.processDone:
		c.mu.Lock()
		errStr := ""
		if c.processErr != nil {
			errStr = c.processErr.Error()
		}
		c.mu.Unlock()
		// Добавляем exit code и сигнал из ProcessState — критично
		// для диагностики OOM-kill (SIGKILL = signal 9) вс. нормального выхода.
		var extra string
		if c.cmd != nil && c.cmd.ProcessState != nil {
			ps := c.cmd.ProcessState
			extra = fmt.Sprintf(" (exit_code=%d", ps.ExitCode())
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					extra += fmt.Sprintf(" signal=%s", ws.Signal())
				}
			}
			extra += ")"
		}
		if errStr != "" {
			return "exited: " + errStr + extra
		}
		return "exited successfully" + extra
	default:
		return ""
	}
}

func (c *Client) handleServerNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
			return
		}
		localPath := c.toLocalPath(p.URI)
		lspDebug("LSP %s: publishDiagnostics: uri=%q local=%q\n",
			c.Language, p.URI, localPath)
		select {
		case c.diagnosticsReady <- localPath:
		default:
		}
	case "language/status":
		// jdtls-специфичное уведомление. Структура: {type, message}.
		// Сигналы готовности к ответам: "ServiceReady" (проект импортирован,
		// classpath построен) и "Started" (legacy/Standard режим).
		// "Starting", "Started" одинаково означают что OSGi контейнер
		// поднят; для invisible-project режима ServiceReady может не приходить,
		// поэтому "Started" тоже принимаем.
		var p struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		lspDebug("LSP %s: language/status: type=%q message=%q\n",
			c.Language, p.Type, p.Message)
		switch p.Type {
		case "ServiceReady", "Started", "Ready":
			if c.javaReadyClosed.CompareAndSwap(false, true) {
				close(c.javaReady)
			}
		}
	}
}

// handleServerRequest отвечает на серверный request минимально валидным
// результатом, чтобы LSP-сервер не блокировался в ожидании ответа клиента.
func (c *Client) handleServerRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		// Сервер (pyright/tsserver) запрашивает настройки для N items.
		// Если вернуть пустой массив — pyright не индексирует проект и не
		// находит definition. Возвращаем массив той же длины с разумными
		// дефолтами для python/typescript.
		n := 1
		var p struct {
			Items []struct {
				ScopeURI string `json:"scopeUri"`
				Section  string `json:"section"`
			} `json:"items"`
		}
		if err := json.Unmarshal(params, &p); err == nil && len(p.Items) > 0 {
			n = len(p.Items)
		}
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			section := ""
			if i < len(p.Items) {
				section = p.Items[i].Section
			}
			arr = append(arr, c.configFor(section))
		}
		result = arr
	case "workspace/workspaceFolders":
		result = []map[string]any{{"uri": c.rootURI, "name": "root"}}
	case "client/registerCapability", "client/unregisterCapability":
		result = nil
	case "window/workDoneProgress/create":
		result = nil
	case "workspace/applyEdit":
		result = map[string]any{"applied": false}
	default:
		// Неизвестный метод — отвечаем ошибкой MethodNotFound (-32601),
		// чтобы сервер не висел.
		resp := rpcServerResponse{
			JSONRPC: "2.0",
			ID:      rawID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + method},
		}
		_ = c.writeServerResponse(resp)
		return
	}
	resp := rpcServerResponse{JSONRPC: "2.0", ID: rawID, Result: result}
	_ = c.writeServerResponse(resp)
}

// configFor возвращает дефолтные настройки для запрошенной LSP-сервером
// секции конфигурации (workspace/configuration). Согласно LSP-спецификации,
// элемент массива результата должен соответствовать именно запрошенной
// секции (e.g. для section="python" — содержимое настроек python, а не
// объект {"python": {...}}). Это критично для pyright: с неправильным
// форматом он не индексирует проект и не отвечает на definition/hover.
func (c *Client) configFor(section string) any {
	switch c.Language {
	case "go":
		return goConfigFor(section)
	case "java":
		return javaConfigFor(section)
	case "python":
		return pythonConfigFor(section)
	case "typescript", "javascript":
		return typescriptConfigFor(section)
	}
	_ = section
	return map[string]any{}
}

func (c *Client) writeServerResponse(resp rpcServerResponse) error {
	msg := map[string]any{
		"jsonrpc": resp.JSONRPC,
		"id":      resp.ID,
	}
	if resp.Error != nil {
		msg["error"] = resp.Error
	} else {
		msg["result"] = resp.Result
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return err
	}
	_, err = c.stdin.Write(data)
	return err
}

// FileURI возвращает file:// URI для абсолютного пути.
func FileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

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

func (c *Client) toRemotePath(localPath string) string {
	return localPath
}

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

// DidOpen уведомляет LSP-сервер о содержимом файла.
// Отправляет didOpen только если файл еще не открыт или его содержимое изменилось.
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
		// Небольшая задержка чтобы LSP сервер успел обработать didChangeWatchedFiles
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

// Location — упрощённое представление LSP Location.
type Location struct {
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartChar int    `json:"start_char"`
	EndLine   int    `json:"end_line"`
	EndChar   int    `json:"end_char"`
}

// lspRange представляет диапазон в документе.
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspLocation как приходит от сервера.
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// lspLocationLink (LocationLink) как приходит от сервера для Definition.
type lspLocationLink struct {
	TargetURI            string   `json:"targetUri"`
	TargetRange          lspRange `json:"targetRange"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children"`
}

func (c *Client) locFromLSP(in []lspLocation) []Location {
	out := make([]Location, 0, len(in))
	for _, l := range in {
		out = append(out, Location{
			URI:       FileURI(c.toLocalPath(l.URI)),
			StartLine: l.Range.Start.Line, StartChar: l.Range.Start.Character,
			EndLine: l.Range.End.Line, EndChar: l.Range.End.Character,
		})
	}
	return out
}

func (c *Client) locFromLinks(in []lspLocationLink) []Location {
	out := make([]Location, 0, len(in))
	for _, l := range in {
		out = append(out, Location{
			URI:       FileURI(c.toLocalPath(l.TargetURI)),
			StartLine: l.TargetSelectionRange.Start.Line, StartChar: l.TargetSelectionRange.Start.Character,
			EndLine: l.TargetSelectionRange.End.Line, EndChar: l.TargetSelectionRange.End.Character,
		})
	}
	return out
}

// Definition — go-to-definition. Если definition вернул пусто (например,
// для TS const/var или Java `new Foo()` когда курсор на скобке), пробуем
// declaration, typeDefinition, а затем повторяем запросы со сдвинутой
// позицией на ближайший идентификатор в строке.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/definition", params, &raw); err != nil {
		lspDebug("LSP %s: Definition ERROR: %v\n", c.Language, err)
		return nil, err
	}
	locs := c.decodeLocations(raw)
	lspDebug("LSP %s: Definition RESULT: locations=%d raw=%q\n", c.Language, len(locs), string(raw))
	if len(locs) > 0 {
		return locs, nil
	}

	// Пробуем declaration и typeDefinition как фоллбэк
	for _, method := range []string{"textDocument/declaration", "textDocument/typeDefinition"} {
		var r json.RawMessage
		if err := c.Call(ctx, method, params, &r); err != nil {
			continue
		}
		if locs := c.decodeLocations(r); len(locs) > 0 {
			lspDebug("LSP %s: %s RESULT: locations=%d\n", c.Language, method, len(locs))
			return locs, nil
		}
	}

	// Эвристика: если мы на краю слова или внутри, пробуем найти начало идентификатора.
	// Часто клик/курсор попадает на скобку после имени или в конец слова, где LSP может не сработать.
	lineText := c.localFileLine(path, line)
	if lineText != "" && character > 0 {
		newChar := character
		// Если текущий символ не буква/цифра, пробуем шагнуть назад
		if character >= len(lineText) || !isAlphaNum(lineText[character]) {
			newChar--
		}
		// Ищем начало слова
		for newChar > 0 && isAlphaNum(lineText[newChar]) {
			newChar--
		}
		if newChar < character {
			if !isAlphaNum(lineText[newChar]) {
				newChar++
			}
			if newChar != character {
				lspDebug("LSP %s: Definition retry at shifted position: %d -> %d\n", c.Language, character, newChar)
				return c.Definition(ctx, path, line, newChar)
			}
		}
	}

	return nil, nil
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// localFileLine читает указанную строку из файла на хосте (а не в контейнере).
// Используется только для эвристик сдвига позиции — ошибки чтения молча
// игнорируются (вернётся пустая строка).
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

// References — find references.
func (c *Client) References(ctx context.Context, path string, line, character int, includeDecl bool) ([]Location, error) {
	params := c.positionParams(path, line, character)
	params["context"] = map[string]any{"includeDeclaration": includeDecl}
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/references", params, &raw); err != nil {
		lspDebug("LSP %s: References ERROR: %v\n", c.Language, err)
		return nil, err
	}
	locs := c.decodeLocations(raw)
	lspDebug("LSP %s: References RESULT: locations=%d raw=%q\n", c.Language, len(locs), string(raw))
	return locs, nil
}

// Hover возвращает текст hover-подсказки (упрощённо — markdown/plain).
func (c *Client) Hover(ctx context.Context, path string, line, character int) (string, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/hover", params, &raw); err != nil {
		lspDebug("LSP %s: Hover ERROR: %v\n", c.Language, err)
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		lspDebug("LSP %s: Hover RESULT: empty\n", c.Language)
		return "", nil
	}
	var h struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		lspDebug("LSP %s: Hover ERROR parse: %v\n", c.Language, err)
		return "", err
	}
	txt := hoverString(h.Contents)
	lspDebug("LSP %s: Hover RESULT: %d chars\n", c.Language, len(txt))
	return txt, nil
}

func (c *Client) decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// LSP может вернуть Location | Location[] | LocationLink[]
	var single lspLocation
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return c.locFromLSP([]lspLocation{single})
	}
	var arr []lspLocation
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 && arr[0].URI != "" {
		return c.locFromLSP(arr)
	}
	var singleLink lspLocationLink
	if err := json.Unmarshal(raw, &singleLink); err == nil && singleLink.TargetURI != "" {
		return c.locFromLinks([]lspLocationLink{singleLink})
	}
	var links []lspLocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 && links[0].TargetURI != "" {
		return c.locFromLinks(links)
	}
	return nil
}

// decodeLocationsWithErr возвращает ошибку если LSP сервер вернул явную ошибку
// или пустой результат с известным сообщением (например "no views").
func (c *Client) decodeLocationsWithErr(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Проверяем не вернул ли сервер ошибку в виде map с message
	var errMap map[string]any
	if err := json.Unmarshal(raw, &errMap); err == nil {
		if msg, ok := errMap["message"].(string); ok {
			return nil, fmt.Errorf("LSP error: %s", msg)
		}
	}
	if locs := c.decodeLocations(raw); len(locs) > 0 {
		return locs, nil
	}
	return nil, nil
}

// positionParams создаёт параметры для запроса на позицию.
func (c *Client) positionParams(path string, line, character int) map[string]any {
	remote := c.toRemotePath(path)
	uri := FileURI(remote)
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}
}

// hoverString преобразует hover contents в строку.
func hoverString(contents any) string {
	if contents == nil {
		return ""
	}
	switch v := contents.(type) {
	case string:
		return v
	case map[string]any:
		if val, ok := v["value"].(string); ok {
			return val
		}
		if val, ok := v["kind"].(string); ok && val == "plaintext" {
			if vval, ok := v["value"].(string); ok {
				return vval
			}
		}
		if val, ok := v["kind"].(string); ok && val == "markdown" {
			if vval, ok := v["value"].(string); ok {
				return vval
			}
		}
	case []any:
		var parts []string
		for _, item := range v {
			if s := hoverString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	data, _ := json.Marshal(contents)
	return string(data)
}

// Implementation — найти реализации интерфейса/метода.
func (c *Client) Implementation(ctx context.Context, path string, line, character int) ([]Location, error) {
	params := c.positionParams(path, line, character)
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/implementation", params, &raw); err != nil {
		return nil, err
	}
	return c.decodeLocations(raw), nil
}
