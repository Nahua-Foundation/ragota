// Package docker — нативный запуск контейнеров через docker CLI
// (без docker-compose). Используется для qdrant, описанных в
// config.DockerConfig.
package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"aitools/internal/config"
)

// Runner запускает контейнеры через docker CLI.
type Runner struct {
	WorkingDir string // используется для разрешения относительных volume-путей
	Cfg        config.DockerConfig
}

// New создаёт нативный Runner из секции docker конфига.
func New(workingDir string, cfg config.DockerConfig) *Runner {
	return &Runner{WorkingDir: workingDir, Cfg: cfg}
}

// Available проверяет, что docker доступен.
func Available(ctx context.Context) error {
	c := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("docker unavailable: %w: %s", err, string(out))
	}
	return nil
}

// Up поднимает все сконфигурированные контейнеры (qdrant),
// создаёт сеть.
func (r *Runner) Up(ctx context.Context) error {
	if r.Cfg.Network != "" {
		fmt.Fprintf(os.Stderr, "docker: ensuring network %s\n", r.Cfg.Network)
		if err := r.ensureNetwork(ctx, r.Cfg.Network); err != nil {
			return err
		}
	}
	if r.Cfg.Qdrant.Image != "" {
		// Для сторонних образов типа qdrant просто надеемся на docker run (он сам спуллит).
		fmt.Fprintf(os.Stderr, "docker: starting qdrant (%s)\n", r.Cfg.Qdrant.Image)
		if err := r.runContainer(ctx, r.Cfg.Qdrant); err != nil {
			return fmt.Errorf("qdrant: %w", err)
		}
	}
	return nil
}

func (r *Runner) ensureImage(ctx context.Context, image string) error {
	content, ok := embeddedDockerfiles[image]
	if !ok {
		// Образ не наш, docker run сам попробует его спуллить.
		return nil
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	// Проверяем, есть ли локальный образ, собранный из той же версии embedded Dockerfile.
	c := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{ index .Config.Labels \"ai-tools.dockerfile-sha256\" }}", image)
	out, err := c.Output()
	if err == nil && strings.TrimSpace(string(out)) == hash {
		return nil
	}

	fmt.Fprintf(os.Stderr, "docker: building image %s from embedded Dockerfile...\n", image)

	// Билдим из stdin
	buildCmd := exec.CommandContext(ctx, "docker", "build", "--label", "ai-tools.dockerfile-sha256="+hash, "-t", image, "-")
	buildCmd.Stdin = bytes.NewReader(content)
	// Перенаправляем вывод в stderr, чтобы пользователь видел прогресс билда
	buildCmd.Stdout = os.Stderr
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}
	return nil
}

// Down останавливает контейнеры (но не удаляет volumes).
func (r *Runner) Down(ctx context.Context) error {
	if r.Cfg.Qdrant.Name != "" {
		_ = exec.CommandContext(ctx, "docker", "stop", r.Cfg.Qdrant.Name).Run()
	}
	return nil
}

// PsService — статус одного контейнера.
type PsService struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// Ps возвращает статусы известных контейнеров (qdrant).
func (r *Runner) Ps(ctx context.Context) ([]PsService, error) {
	var result []PsService
	if r.Cfg.Qdrant.Name != "" {
		result = append(result, r.inspectPs(ctx, r.Cfg.Qdrant.Name))
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
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	OpenStdin bool   `json:"OpenStdin"`
	Image     string `json:"Image"`
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
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", name).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "network", "create", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create network %s: %w: %s", name, err, string(out))
	}
	return nil
}

var proxyVars = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
}

// runContainer запускает контейнер: если уже существует — стартует, иначе docker run -di.
func (r *Runner) runContainer(ctx context.Context, c config.DockerContainerCfg) error {
	if c.Name == "" || c.Image == "" {
		return nil
	}
	// Если уже есть — проверим состояние.
	if st, err := r.inspectState(ctx, c.Name); err == nil {
		if id, err := imageID(ctx, c.Image); err == nil && st.Image != "" && st.Image != id {
			fmt.Fprintf(os.Stderr, "docker: container %s uses outdated image, recreating...\n", c.Name)
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Name).Run()
		} else
		// Если контейнер в статусе restarting или создан без -i (OpenStdin=false),
		// значит он падает при старте из-за отсутствия stdin. Пересоздаём.
		if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			fmt.Fprintf(os.Stderr, "docker: container %s is in bad state (%s, stdin=%v), recreating...\n", c.Name, st.Status, st.OpenStdin)
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Name).Run()
		} else {
			if st.Running {
				return nil
			}
			out, err := exec.CommandContext(ctx, "docker", "start", c.Name).CombinedOutput()
			if err != nil {
				return fmt.Errorf("docker start %s: %w: %s", c.Name, err, string(out))
			}
			return nil
		}
	}
	args := []string{"run", "-di", "--name", c.Name, "--restart", "unless-stopped"}
	network := c.Network
	if network == "" {
		network = r.Cfg.Network
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	for _, p := range c.Ports {
		args = append(args, "-p", p)
	}
	for _, v := range c.Volumes {
		args = append(args, "-v", r.resolveVolume(v))
	}

	env := append([]string{}, c.Env...)
	// Автоматически прокидываем переменные прокси и сертификатов из окружения хоста,
	// если они не заданы явно в конфигурации контейнера.
	hostCertsMounted := false
	for _, v := range proxyVars {
		if val := os.Getenv(v); val != "" {
			found := false
			for _, existing := range c.Env {
				if strings.HasPrefix(existing, v+"=") {
					found = true
					break
				}
			}
			if !found {
				env = append(env, v+"="+val)
				// Если переменная указывает на файл/папку сертификатов, монтируем её.
				if strings.Contains(v, "CERT") || strings.Contains(v, "BUNDLE") {
					if _, err := os.Stat(val); err == nil {
						args = append(args, "-v", val+":"+val+":ro")
						hostCertsMounted = true
					}
				}
			}
		}
	}

	// На macOS пробрасываем системный бандл, если есть прокси, но не заданы сертификаты.
	if runtime.GOOS == "darwin" && !hostCertsMounted {
		hasProxy := os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != ""
		if hasProxy {
			macCert := "/etc/ssl/cert.pem"
			if _, err := os.Stat(macCert); err == nil {
				args = append(args, "-v", macCert+":"+macCert+":ro")
				env = append(env, "SSL_CERT_FILE="+macCert)
			}
		}
	}

	for _, e := range env {
		args = append(args, "-e", e)
	}

	args = append(args, c.Image)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", c.Name, err, string(out))
	}
	return nil
}

// resolveVolume превращает относительный host-путь в абсолютный (относительно WorkingDir).
func (r *Runner) resolveVolume(v string) string {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return v
	}
	host := parts[0]
	if !filepath.IsAbs(host) && !strings.HasPrefix(host, "~") {
		host = filepath.Join(r.WorkingDir, host)
	}
	return host + ":" + parts[1]
}
