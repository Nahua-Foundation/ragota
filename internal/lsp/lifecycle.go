package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Файл реализует жизненный цикл LSP-клиента: запуск процесса, handshake
// (initialize/initialized), завершение и сводки stderr/процесса для
// диагностики. См. также `jsonrpc.go` для транспорта и `navigation.go`
// для запросов навигации.

// startDocker запускает LSP-сервер в Docker-контейнере через docker exec.
// Контейнер должен быть уже запущен (управляется internal/docker.Runner).
func startDocker(ctx context.Context, spec ServerSpec, root string) (*Client, error) {
	// Для Docker-режима используем stdio через docker exec
	// Все LSP запускаются в едином контейнере ragota-lsp
	containerName := "ragota-lsp"

	// Команда: docker exec -i -w /workspace <container> <command> <args>
	args := make([]string, 0, len(spec.Args)+5)
	args = append(args, "exec", "-i", "-w", "/workspace", containerName)
	args = append(args, spec.Command)
	args = append(args, spec.Args...)

	_ = ctx // Не используем ctx для отмены процесса (см. комментарий в Start)
	cmd := exec.Command("docker", args...)
	cmd.Dir = root

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
		return nil, fmt.Errorf("start docker exec %s: %w", spec.Language, err)
	}

	// В docker-режиме корень проекта смонтирован в контейнере по фиксированному
	// пути (-w /workspace), поэтому LSP-сервер должен видеть rootUri именно
	// как путь внутри контейнера. Вычисляем относительный путь от spec.LocalRoot
	// к root и добавляем к remoteRoot — это позволяет запускать отдельные
	// LSP-инстансы для каждого подпроекта (go.mod, package.json и т.д.).
	const remoteRoot = "/workspace"
	hostRoot, absErr := filepath.Abs(root)
	if absErr != nil {
		hostRoot = root
	}
	localRoot := spec.LocalRoot
	if localRoot == "" {
		localRoot = hostRoot
	}
	relPath, relErr := filepath.Rel(localRoot, hostRoot)
	remoteWorkspace := remoteRoot
	if relErr == nil && relPath != "" && relPath != "." {
		remoteWorkspace = filepath.Join(remoteRoot, relPath)
	}
	rootURI := FileURI(remoteWorkspace)

	c := &Client{
		Language:         spec.Language,
		cmd:              cmd,
		stdin:            stdin,
		stdout:           stdout,
		stderr:           stderr,
		pending:          make(map[int64]chan rpcResponse),
		rootURI:          rootURI,
		localRoot:        localRoot,
		hostRoot:         hostRoot,
		remoteRoot:       remoteWorkspace, // ВАЖНО: remoteRoot = workspace в контейнере, не /workspace
		openedFiles:      make(map[string]string),
		openedVers:       make(map[string]int),
		diagnosticsReady: make(chan string, 16),
		processDone:      make(chan struct{}),
		javaReady:        make(chan struct{}),
		goplsReady:       make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.processErr = err
		c.mu.Unlock()
		close(c.processDone)
		details := c.processSummary()
		tail := c.stderrSummary()
		lspDebug("LSP %s (docker): process exited: %s; stderr tail: %s\n",
			c.Language, details, tail)
	}()
	go c.readLoop()
	go c.consumeStderr(stderr, spec.Language)

	if err := c.initialize(ctx, root); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s (docker): %w", spec.Language, err)
	}
	return c, nil
}

// Start запускает процесс LSP-сервера и выполняет initialize/initialized.
func Start(ctx context.Context, spec ServerSpec, root string) (*Client, error) {
	// В режиме Docker запускаем сервер через docker exec
	if spec.IsDocker {
		return startDocker(ctx, spec, root)
	}

	// Обрабатываем относительные пути в аргументах (например, -data .ragota/jdtls-data)
	// так как рабочий каталог процесса совпадает с рабочим каталогом ragota,
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
		goplsReady:       make(chan struct{}),
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
	go c.consumeStderr(stderr, spec.Language)

	if err := c.initialize(ctx, root); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s: %w", spec.Language, err)
	}
	return c, nil
}

// consumeStderr читает stderr процесса LSP-сервера, отбрасывает «шумные»
// не-ошибочные строки (JVM warnings, JUL INFO/FINE/CONFIG, SLF4J и т.п.)
// и логирует только реальные ошибки/предупреждения.
//
// ВАЖНО: фильтр применяется и к rememberStderr — иначе реальные ошибки jdtls
// (Exception/SEVERE/!ENTRY) вытесняются десятками WARNING'ов JVM из буфера,
// и в recent stderr мы видим только шум.
func (c *Client) consumeStderr(stderr interface{ Read([]byte) (int, error) }, language string) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
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
		if language == "java" {
			lspDebug("LSP %s stderr: %s\n", language, line)
		} else {
			lspDebug("LSP %s stderr: %s", language, line)
		}
	}
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

// initialize выполняет LSP-handshake: отправляет initialize + initialized,
// затем рассылает didChangeConfiguration для серверов, читающих настройки
// уже на этом этапе (pyright/tsserver), и дожидается готовности jdtls.
func (c *Client) initialize(ctx context.Context, root string) error {
	_ = root
	// processId должен быть PID родительского процесса, который сервер мониторит,
	// чтобы завершиться при его смерти (через kill(pid, 0)).
	// Отключаем мониторинг для всех LSP, чтобы они не закрывались при разрыве
	// соединения с MCP клиентом (SSE/stdio).
	var processID any = nil
	// gopls/jdtls требуют корректного rootUri и workspaceFolders для индексации.
	params := map[string]any{
		"processId":    processID,
		"rootPath":     pathFromFileURI(c.rootURI),
		"rootUri":      c.rootURI,
		"capabilities": c.capabilities(),
		"workspaceFolders": []map[string]any{
			{"uri": c.rootURI, "name": filepath.Base(pathFromFileURI(c.rootURI))},
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
	// gopls после инициализации сразу готов к работе с основным воркспейсом.
	// Ранее здесь отправлялся didChangeWorkspaceFolders, что приводило к дублированию
	// view в gopls и ошибкам "no views".
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
			// caller ctx может иметь deadline < 120s (например 60s из lspsrv).
			// Для Java используем detached context — jdtls инициализация фоновая.
			lspDebug("LSP java: caller context cancelled, continuing background init\n")
			// Не возвращаем ошибку — jdtls продолжает инициализироваться в фоне.
		case <-time.After(readyTimeout):
			lspDebug("LSP java: ready timeout after %s, continuing anyway\n",
				readyTimeout)
		}
	}
	c.initialized.Store(true)

	if c.Language == "go" {
		// gopls сигнализирует о завершении начальной индексации через $/progress.
		// Ждем этого сигнала, чтобы первые запросы не возвращали пустые результаты.
		// Таймаут 30с обычно достаточно даже для больших проектов.
		select {
		case <-c.goplsReady:
			lspDebug("LSP go: initial indexing finished\n")
		case <-time.After(30 * time.Second):
			lspDebug("LSP go: indexing wait timeout, continuing\n")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

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

// rememberStderr сохраняет последние ~200 строк stderr для последующего
// включения в сообщения об ошибках.
func (c *Client) rememberStderr(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stderrLines = append(c.stderrLines, line)
	if len(c.stderrLines) > 200 {
		c.stderrLines = c.stderrLines[len(c.stderrLines)-200:]
	}
}

// stderrSummary возвращает «хвост» stderr в виде одной строки.
func (c *Client) stderrSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.stderrLines, " | ")
}

// withStderr оборачивает ошибку диагностикой процесса + хвостом stderr.
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

// withStderrLocked — версия withStderr, вызываемая под удержанием c.mu.
// Не пытается заново брать мьютекс (важно, чтобы избежать дедлока в write()).
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

// processSummary возвращает короткое описание состояния процесса:
// exit code, сигнал (SIGKILL/SIGTERM/...), текст ошибки os/exec.
// Пустая строка означает «процесс ещё работает». Критично для
// диагностики OOM-kill: SIGKILL (signal 9) от OOM-killer выглядит как
// внезапная пропажа сервера, и без этих данных причину не понять.
func (c *Client) processSummary() string {
	select {
	case <-c.processDone:
		c.mu.Lock()
		errStr := ""
		if c.processErr != nil {
			errStr = c.processErr.Error()
		}
		c.mu.Unlock()
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
