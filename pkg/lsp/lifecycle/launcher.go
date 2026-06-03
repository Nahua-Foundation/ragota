package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ragota/pkg/lsp"
	"ragota/pkg/lsp/jsonrpc"
	"ragota/pkg/lsp/lang"
	"ragota/pkg/lsp/process"
	"ragota/pkg/lsp/session"
)

// Start launches an LSP server process, creates a session, and performs the handshake.
// If spec.IsDocker, launches inside the ragota-lsp Docker container.
func Start(ctx context.Context, spec lsp.ServerSpec, root string) (lsp.Client, error) {
	if spec.IsDocker {
		return startDocker(ctx, spec, root)
	}

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

	cmd := exec.Command(spec.Command, args...)
	cmd.Dir = root

	for i, arg := range args {
		if (arg == "-data" || arg == "--data") && i+1 < len(args) {
			_ = os.MkdirAll(args[i+1], 0755)
		}
	}

	return launch(ctx, cmd, spec.Language, root, "", "")
}

func startDocker(ctx context.Context, spec lsp.ServerSpec, root string) (lsp.Client, error) {
	containerName := "ragota-lsp"

	args := make([]string, 0, len(spec.Args)+5)
	args = append(args, "exec", "-i", "-w", "/workspace", containerName)
	args = append(args, spec.Command)
	args = append(args, spec.Args...)

	cmd := exec.Command("docker", args...)
	cmd.Dir = root

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

	return launch(ctx, cmd, spec.Language, root, hostRoot, remoteWorkspace)
}

// launch is the common path for local and docker launches.
func launch(ctx context.Context, cmd *exec.Cmd, language, root, hostRoot, remoteRoot string) (lsp.Client, error) {
	proc, err := process.New(cmd)
	if err != nil {
		return nil, err
	}
	if err := proc.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	var rootURI, localRoot string
	if hostRoot != "" {
		rootURI = lsp.FileURI(remoteRoot)
		localRoot = hostRoot
	} else {
		rootURI = lsp.FileURI(root)
		localRoot = root
		hostRoot = ""
		remoteRoot = ""
	}

	langReg := lang.Default()
	var langCaps *lang.Capabilities
	if langReg != nil {
		langCaps = langReg.Get(language)
	}

	// Create session (not yet initialized — will be done after conn setup)
	sess := session.New(nil, proc, language, langCaps, rootURI, localRoot, hostRoot, remoteRoot, DebugLog)

	// Create JSON-RPC connection with callbacks pointing to session
	conn := jsonrpc.NewConn(
		proc.Stdin, proc.Stdout,
		language,
		func() { /* onDead: manager handles cleanup */ },
		sess.OnServerRequest,
		sess.OnServerNotification,
	)
	conn.DebugLog = DebugLog

	// Wire the conn into the session
	sess.Conn = conn

	// Start reading server messages
	conn.StartReadLoop()
	proc.ConsumeStderr(language)

	// Monitor process exit to fail pending requests
	go func() {
		_ = proc.Wait()
		details := proc.ProcessSummary()
		tail := proc.StderrSummary()
		DebugLog("LSP %s: process exited: %s; stderr tail: %s\n", language, details, tail)
		conn.FailPending(fmt.Errorf("lsp %s process exited: %s", language, details))
	}()

	// Perform LSP handshake
	if err := Initialize(ctx, sess, root); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("initialize %s: %w", language, err)
	}

	return sess, nil
}
