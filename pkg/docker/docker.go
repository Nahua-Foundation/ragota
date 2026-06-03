// Package docker — нативный запуск контейнеров через docker CLI.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ragota/pkg/config"
	"ragota/pkg/logger"
)

const cliTimeout = 30 * time.Second

func withCLI(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cliTimeout)
}

// Runner запускает контейнеры через docker CLI.
type Runner struct {
	WorkingDir string
	Cfg        config.DockerConfig
}

// New создаёт нативный Runner.
func New(workingDir string, cfg config.DockerConfig) *Runner {
	return &Runner{WorkingDir: workingDir, Cfg: cfg}
}

// Available проверяет, что docker доступен.
func Available(ctx context.Context) error {
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	c := exec.CommandContext(tCtx, "docker", "version", "--format", "{{.Server.Version}}")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("docker unavailable: %w: %s", err, string(out))
	}
	return nil
}

// Up поднимает все сконфигурированные контейнеры.
func (r *Runner) Up(ctx context.Context, all bool) error {
	tCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = tCtx
	if r.Cfg.Network != "" {
		logger.Log().Debug().Str("network", r.Cfg.Network).Msg("docker: ensuring network")
		if err := r.ensureNetwork(ctx, r.Cfg.Network); err != nil {
			return err
		}
	}
	if err := r.ensureVolumes(); err != nil {
		return fmt.Errorf("ensure volumes: %w", err)
	}
	if r.Cfg.Qdrant.Image != "" {
		logger.Log().Info().Str("image", r.Cfg.Qdrant.Image).Msg("docker: starting qdrant")
		if err := r.runContainer(ctx, r.Cfg.Qdrant); err != nil {
			return fmt.Errorf("qdrant: %w", err)
		}
	}
	if all && r.Cfg.LSP.Image != "" {
		logger.Log().Info().Str("image", r.Cfg.LSP.Image).Msg("docker: starting lsp container")
		if err := r.runLSPContainer(ctx, r.Cfg.LSP); err != nil {
			return fmt.Errorf("lsp: %w", err)
		}
	}
	return nil
}

// WaitQdrant блокируется, пока Qdrant не ответит на /readyz.
func (r *Runner) WaitQdrant(ctx context.Context) error {
	hostPort := extractHostPort(r.Cfg.Qdrant.Ports)
	if hostPort == "" {
		return nil
	}
	url := "http://" + hostPort + "/readyz"
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func extractHostPort(ports []string) string {
	for _, p := range ports {
		parts := strings.Split(p, ":")
		if len(parts) >= 2 {
			host := parts[0]
			if host == "" {
				host = "127.0.0.1"
			}
			if len(parts) == 3 {
				return host + ":" + parts[1]
			}
			return "127.0.0.1:" + parts[0]
		}
	}
	return ""
}

// Down останавливает контейнеры.
func (r *Runner) Down(ctx context.Context) error {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if r.Cfg.Qdrant.Name != "" {
		_ = exec.CommandContext(dCtx, "docker", "stop", "-t", "3", r.Cfg.Qdrant.Name).Run()
	}
	if r.Cfg.LSP.Image != "" {
		_ = exec.CommandContext(dCtx, "docker", "stop", "-t", "3", "ragota-lsp").Run()
	}
	return nil
}

// PsService — статус одного контейнера.
type PsService struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// Ps возвращает статусы известных контейнеров.
func (r *Runner) Ps(ctx context.Context) ([]PsService, error) {
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	var result []PsService
	if r.Cfg.Qdrant.Name != "" {
		result = append(result, r.inspectPs(tCtx, r.Cfg.Qdrant.Name))
	}
	if r.Cfg.LSP.Image != "" {
		result = append(result, r.inspectPs(tCtx, "ragota-lsp"))
	}
	return result, nil
}

func (r *Runner) inspectPs(ctx context.Context, name string) PsService {
	st, err := r.inspectState(ctx, name)
	if err != nil {
		return PsService{Name: name, State: "absent"}
	}
	return PsService{Name: name, State: st.Status, Status: fmt.Sprintf("running=%v", st.Running)}
}

type containerState struct {
	Status    string   `json:"Status"`
	Running   bool     `json:"Running"`
	OpenStdin bool     `json:"OpenStdin"`
	Image     string   `json:"Image"`
	Mounts    []string `json:"Mounts"`
}

func (r *Runner) inspectState(ctx context.Context, name string) (*containerState, error) {
	format := `{"Status":"{{.State.Status}}","Running":{{.State.Running}},"OpenStdin":{{.Config.OpenStdin}},"Image":"{{.Image}}"}`
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, name).Output()
	if err != nil {
		return nil, err
	}
	var st containerState
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return nil, err
	}
	mountFormat := `{{range .Mounts}}{{.Source}}:{{.Destination}}
{{end}}`
	mountOut, err := exec.CommandContext(ctx, "docker", "inspect", "--format", mountFormat, name).Output()
	if err == nil {
		for _, line := range strings.Split(string(mountOut), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				st.Mounts = append(st.Mounts, line)
			}
		}
	}
	return &st, nil
}

func imageID(ctx context.Context, image string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Runner) ensureNetwork(ctx context.Context, name string) error {
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	if err := exec.CommandContext(tCtx, "docker", "network", "inspect", name).Run(); err == nil {
		return nil
	}
	tCtx2, cancel2 := withCLI(ctx)
	defer cancel2()
	out, err := exec.CommandContext(tCtx2, "docker", "network", "create", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create network %s: %w: %s", name, err, string(out))
	}
	return nil
}

func (r *Runner) ensureVolumes() error {
	for _, d := range []string{".ragota/qdrant_storage"} {
		abs := filepath.Join(r.WorkingDir, d)
		if err := os.MkdirAll(abs, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", abs, err)
		}
	}
	return nil
}

// resolveVolume превращает относительный host-путь в абсолютный.
func (r *Runner) resolveVolume(v string) string {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return v
	}
	host := parts[0]
	if !filepath.IsAbs(host) {
		if strings.HasPrefix(host, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				host = filepath.Join(home, strings.TrimPrefix(host, "~/"))
			}
		} else {
			host = filepath.Join(r.WorkingDir, host)
		}
	}
	return host + ":" + parts[1]
}
