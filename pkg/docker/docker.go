// Package docker — нативный запуск контейнеров через docker CLI.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ragota/pkg/logger"
)

const cliTimeout = 30 * time.Second

func withCLI(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cliTimeout)
}

// Runner запускает контейнеры через docker CLI.
type Runner struct {
	WorkingDir string
	QdrantPort int // динамически назначенный порт
}

// New создаёт Runner.
func New(workingDir string) *Runner {
	return &Runner{WorkingDir: workingDir}
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

// findFreePort находит свободный TCP-порт на localhost.
func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// getExistingPort читает host-порт из существующего контейнера.
// Пробует два метода: docker port (быстрый, fallback) и docker inspect (детальный).
func (r *Runner) getExistingPort(containerName string) (int, error) {
	ctx, cancel := withCLI(context.Background())
	defer cancel()

	// Метод 1: docker port — простой и надёжный
	if port := r.getPortViaDockerPort(ctx, containerName); port > 0 {
		return port, nil
	}

	// Метод 2: docker inspect --format — парсим JSON с маппингом портов
	if port := r.getPortViaInspect(ctx, containerName); port > 0 {
		return port, nil
	}

	return 0, fmt.Errorf("could not determine host port for container %s (6333/tcp)", containerName)
}

// getPortViaDockerPort использует `docker port` для получения host-порта.
func (r *Runner) getPortViaDockerPort(ctx context.Context, containerName string) int {
	out, err := exec.CommandContext(ctx, "docker", "port", containerName, "6333").Output()
	if err != nil {
		return 0
	}
	// Формат вывода: "127.0.0.1:PORT"
	line := strings.TrimSpace(string(out))
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return 0
	}
	port, err := strconv.Atoi(line[idx+1:])
	if err != nil {
		return 0
	}
	return port
}

// getPortViaInspect использует `docker inspect --format` для получения host-порта.
func (r *Runner) getPortViaInspect(ctx context.Context, containerName string) int {
	format := `{{json .NetworkSettings.Ports}}`
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, containerName).Output()
	if err != nil {
		return 0
	}

	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(out, &ports); err != nil {
		return 0
	}

	if bindings, ok := ports["6333/tcp"]; ok && len(bindings) > 0 {
		port, err := strconv.Atoi(bindings[0].HostPort)
		if err == nil {
			return port
		}
	}
	return 0
}

// Up поднимает все контейнеры.
func (r *Runner) Up(ctx context.Context) error {
	// Создаём сеть если нет
	network := "ragota-net"
	if err := r.ensureNetwork(ctx, network); err != nil {
		return err
	}

	if err := r.ensureVolumes(); err != nil {
		return fmt.Errorf("ensure volumes: %w", err)
	}

	// Запускаем Qdrant — порт определяется внутри runQdrant
	if err := r.runQdrant(ctx, network); err != nil {
		return fmt.Errorf("qdrant: %w", err)
	}

	// Guard: QdrantPort должен быть определён
	if r.QdrantPort == 0 {
		return fmt.Errorf("qdrant: host port not determined after startup")
	}

	// Запускаем LSP контейнер
	if err := r.runLSPContainer(ctx, network); err != nil {
		logger.Log().Warn().Err(err).Msg("docker: LSP container failed (non-fatal)")
	}

	return nil
}

// QdrantURL возвращает URL для подключения к Qdrant.
func (r *Runner) QdrantURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", r.QdrantPort)
}

// WaitQdrant блокируется, пока Qdrant не ответит на /readyz.
func (r *Runner) WaitQdrant(ctx context.Context) error {
	url := r.QdrantURL() + "/readyz"
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

// Down останавливает контейнеры.
func (r *Runner) Down(ctx context.Context) error {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(dCtx, "docker", "stop", "-t", "3", "ragota-qdrant").Run()
	_ = exec.CommandContext(dCtx, "docker", "stop", "-t", "3", "ragota-lsp").Run()
	return nil
}

// PsService — статус одного контейнера.
type PsService struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// Ps возвращает статусы контейнеров.
func (r *Runner) Ps(ctx context.Context) ([]PsService, error) {
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	var result []PsService
	for _, name := range []string{"ragota-qdrant", "ragota-lsp"} {
		result = append(result, r.inspectPs(tCtx, name))
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
