// Package process управляет OS-процессом LSP-сервера: запуск, мониторинг, остановка.
package process

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Process wraps an exec.Cmd with its stdio pipes and lifecycle management.
type Process struct {
	Cmd    *exec.Cmd
	Stdin  Writer
	Stdout Reader
	Stderr Reader

	mu          sync.Mutex
	stderrLines []string
	processErr  error
	processDone chan struct{}
	started     bool
}

// Writer is an io.WriteCloser (satisfied by io.WriteCloser).
type Writer interface {
	Write(p []byte) (n int, err error)
	Close() error
}

// Reader is an io.ReadCloser (satisfied by io.ReadCloser).
type Reader interface {
	Read(p []byte) (n int, err error)
	Close() error
}

// New wraps an already-configured exec.Cmd. The caller must call Start() to run it.
func New(cmd *exec.Cmd) (*Process, error) {
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
	return &Process{
		Cmd:         cmd,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		processDone: make(chan struct{}),
	}, nil
}

// Start launches the process and begins monitoring it in the background.
func (p *Process) Start() error {
	if err := p.Cmd.Start(); err != nil {
		return err
	}
	p.started = true
	go func() {
		err := p.Cmd.Wait()
		p.mu.Lock()
		p.processErr = err
		p.mu.Unlock()
		close(p.processDone)
	}()
	return nil
}

// Wait blocks until the process exits and returns its error (if any).
func (p *Process) Wait() error {
	if !p.started {
		return fmt.Errorf("process not started")
	}
	<-p.processDone
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.processErr
}

// IsAlive returns true if the process is still running.
func (p *Process) IsAlive() bool {
	if !p.started {
		return false
	}
	select {
	case <-p.processDone:
		return false
	default:
		return true
	}
}

// Kill sends SIGKILL to the process (best-effort).
func (p *Process) Kill() {
	if p.Cmd.Process != nil {
		_ = p.Cmd.Process.Kill()
	}
}

// ConsumeStderr reads stderr in the background, filtering out noisy log lines.
// Should be called after Start().
func (p *Process) ConsumeStderr(language string) {
	go p.consumeStderr(language)
}

// StderrSummary returns the last few stderr lines (for diagnostics).
func (p *Process) StderrSummary() string {
	p.mu.Lock()
	lines := make([]string, len(p.stderrLines))
	copy(lines, p.stderrLines)
	p.mu.Unlock()
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, "; ")
}

// ProcessSummary returns a human-readable description of the process state.
func (p *Process) ProcessSummary() string {
	if !p.started {
		return "not started"
	}
	select {
	case <-p.processDone:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.processErr != nil {
			return fmt.Sprintf("exited: %v", p.processErr)
		}
		return "exited normally"
	default:
		return "running"
	}
}

// ProcessDone returns a channel that is closed when the process exits.
func (p *Process) ProcessDone() <-chan struct{} {
	return p.processDone
}

// ProcessErr returns the process exit error (nil if still running or exited normally).
func (p *Process) ProcessErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.processErr
}

// rememberStderr appends a line to the circular stderr buffer.
func (p *Process) rememberStderr(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stderrLines = append(p.stderrLines, line)
	if len(p.stderrLines) > 50 {
		p.stderrLines = p.stderrLines[len(p.stderrLines)-50:]
	}
}

func (p *Process) consumeStderr(language string) {
	scanner := bufio.NewScanner(p.Stderr)
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
			strings.HasPrefix(trimmed, "Picked up") ||
			strings.HasPrefix(trimmed, "SLF4J") ||
			strings.HasPrefix(trimmed, "WARNING: sun.reflect") ||
			strings.HasPrefix(trimmed, "NOTE: Picked up JDK_JAVA_OPTIONS")
		if noisy {
			continue
		}
		p.rememberStderr(trimmed)
	}
}
