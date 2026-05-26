// Package docker — нативный запуск контейнеров через docker CLI
// (без docker-compose). Используется для qdrant, описанных в
// config.DockerConfig.
package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"ragota/internal/config"
	"ragota/internal/logger"
	"runtime"
	"strings"
)

//go:embed Dockerfile.lsp
var lspDockerfile []byte

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

// Up поднимает все сконфигурированные контейнеры.
// Если all=true — запускает все сервисы (qdrant, lsp), иначе только qdrant.
func (r *Runner) Up(ctx context.Context, all bool) error {
	if r.Cfg.Network != "" {
		logger.Log().Debug().Str("network", r.Cfg.Network).Msg("docker: ensuring network")
		if err := r.ensureNetwork(ctx, r.Cfg.Network); err != nil {
			return err
		}
	}
	// Создаём директории для томов перед запуском контейнеров
	if err := r.ensureVolumes(); err != nil {
		return fmt.Errorf("ensure volumes: %w", err)
	}
	// Qdrant запускается всегда
	if r.Cfg.Qdrant.Image != "" {
		logger.Log().Info().Str("image", r.Cfg.Qdrant.Image).Msg("docker: starting qdrant")
		if err := r.runContainer(ctx, r.Cfg.Qdrant); err != nil {
			return fmt.Errorf("qdrant: %w", err)
		}
	}
	// LSP только в режиме --env docker
	if all {
		// Запускаем единый LSP-контейнер
		if r.Cfg.LSP.Image != "" {
			logger.Log().Info().Str("image", r.Cfg.LSP.Image).Msg("docker: starting lsp container")
			if err := r.runLSPContainer(ctx, r.Cfg.LSP); err != nil {
				return fmt.Errorf("lsp: %w", err)
			}
		}
	}
	return nil
}

// ensureVolumes создаёт директории для томов Docker на хосте.
func (r *Runner) ensureVolumes() error {
	dirs := []string{
		".ragota/qdrant_storage",
	}
	for _, d := range dirs {
		abs := filepath.Join(r.WorkingDir, d)
		if err := os.MkdirAll(abs, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", abs, err)
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
	c := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{ index .Config.Labels \"ragota.dockerfile-sha256\" }}", image)
	out, err := c.Output()
	if err == nil && strings.TrimSpace(string(out)) == hash {
		return nil
	}

	logger.Log().Info().Str("image", image).Msg("docker: building image from embedded Dockerfile")

	// Билдим из stdin
	buildCmd := exec.CommandContext(ctx, "docker", "build", "--label", "ragota.dockerfile-sha256="+hash, "-t", image, "-")
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
		_ = exec.CommandContext(ctx, "docker", "stop", "-t", "3", r.Cfg.Qdrant.Name).Run()
	}
	// Единый LSP-контейнер
	if r.Cfg.LSP.Image != "" {
		_ = exec.CommandContext(ctx, "docker", "stop", "-t", "3", "ragota-lsp").Run()
	}
	return nil
}

// PsService — статус одного контейнера.
type PsService struct {
	Name   string `json:"Name"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// Ps возвращает статусы известных контейнеров (qdrant, lsp).
func (r *Runner) Ps(ctx context.Context) ([]PsService, error) {
	var result []PsService
	if r.Cfg.Qdrant.Name != "" {
		result = append(result, r.inspectPs(ctx, r.Cfg.Qdrant.Name))
	}
	// Единый LSP-контейнер
	if r.Cfg.LSP.Image != "" {
		result = append(result, r.inspectPs(ctx, "ragota-lsp"))
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
	Mounts    []string `json:"Mounts"` // resolved host:container paths
}

func (r *Runner) inspectState(ctx context.Context, name string) (*containerState, error) {
	// Сначала базовый inspect
	format := `{"Status":"{{.State.Status}}","Running":{{.State.Running}},"OpenStdin":{{.Config.OpenStdin}},"Image":"{{.Image}}"}`
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, name).Output()
	if err != nil {
		return nil, err
	}
	var st containerState
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return nil, err
	}

	// Получаем mount points
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
			logger.Log().Warn().Str("container", c.Name).Msg("docker: container uses outdated image, recreating")
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Name).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			logger.Log().Warn().Str("container", c.Name).Str("status", st.Status).Msg("docker: container in bad state, recreating")
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Name).Run()
		} else if r.volumesMismatch(st, c.Volumes) {
			logger.Log().Warn().Str("container", c.Name).Msg("docker: container has stale volume mounts, recreating")
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
	args := []string{"run", "-di", "--init", "--name", c.Name, "--restart", "unless-stopped"}
	network := c.Network
	if network == "" {
		network = r.Cfg.Network
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	// Проброс GPU для Ollama (nvidia, amd, macos)
	if c.GPU != "" {
		args = append(args, "--gpus", c.GPU)
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

// volumesMismatch проверяет, отличаются ли resolved volumes от текущих mount'ов контейнера.
func (r *Runner) volumesMismatch(st *containerState, volumes []string) bool {
	if len(st.Mounts) == 0 || len(volumes) == 0 {
		return false
	}
	existing := make(map[string]bool)
	for _, m := range st.Mounts {
		existing[m] = true
	}
	for _, v := range volumes {
		resolved := r.resolveVolume(v)
		if !existing[resolved] {
			return true
		}
	}
	return false
}

// resolveVolume превращает относительный host-путь в абсолютный (относительно WorkingDir).
// Поддерживает пути вида ".ragota/...", "./...", а также "~" для домашней директории.
func (r *Runner) resolveVolume(v string) string {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return v
	}
	host := parts[0]
	if !filepath.IsAbs(host) {
		// Обрабатываем ~ для домашней директории
		if strings.HasPrefix(host, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				host = filepath.Join(home, strings.TrimPrefix(host, "~/"))
			}
		} else {
			// Относительный путь разрешаем относительно WorkingDir
			host = filepath.Join(r.WorkingDir, host)
		}
	}
	return host + ":" + parts[1]
}

// ensureLSPImage проверяет наличие образа ragota-lsp и билдит его при необходимости.
// Образ не скачивается с Docker Hub — только локальная сборка из Dockerfile.
func (r *Runner) ensureLSPImage(ctx context.Context, image string) error {
	// Проверяем, есть ли образ локально
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	if err := cmd.Run(); err == nil {
		logger.Log().Debug().Str("image", image).Msg("docker: image found locally")
		return nil
	}

	// Образа нет — билдим локально из Dockerfile
	logger.Log().Info().Str("image", image).Msg("docker: image not found, building from Dockerfile")
	return r.buildLSPImage(ctx, image)
}

// buildLSPImage билдит образ ragota-lsp из встроенного Dockerfile.
// Dockerfile встраивается в бинарь через //go:embed.
func (r *Runner) buildLSPImage(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", image, "-")
	cmd.Stdin = bytes.NewReader(lspDockerfile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	logger.Log().Info().Str("image", image).Msg("docker: image built successfully")
	return nil
}

// runLSPContainer запускает единый LSP-контейнер со всеми серверами.
// Образ ragota-lsp билдится из встроенного Dockerfile при отсутствии.
func (r *Runner) runLSPContainer(ctx context.Context, cfg config.LSPDockerCfg) error {
	if cfg.Image == "" {
		return nil
	}
	containerName := "ragota-lsp"

	// Сначала убеждаемся, что образ существует (билдим при необходимости)
	if err := r.ensureLSPImage(ctx, cfg.Image); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}

	// Проверяем, есть ли уже контейнер
	if st, err := r.inspectState(ctx, containerName); err == nil {
		if id, err := imageID(ctx, cfg.Image); err == nil && st.Image != "" && st.Image != id {
			logger.Log().Warn().Str("container", containerName).Msg("docker: container uses outdated image, recreating")
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			logger.Log().Warn().Str("container", containerName).Str("status", st.Status).Msg("docker: container in bad state, recreating")
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()
		} else if r.volumesMismatch(st, cfg.Volumes) {
			logger.Log().Warn().Str("container", containerName).Msg("docker: container has stale volume mounts, recreating")
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()
		} else {
			if st.Running {
				return nil
			}
			out, err := exec.CommandContext(ctx, "docker", "start", containerName).CombinedOutput()
			if err != nil {
				return fmt.Errorf("docker start %s: %w: %s", containerName, err, string(out))
			}
			return nil
		}
	}

	args := []string{"run", "-di", "--init", "--name", containerName, "--restart", "unless-stopped"}
	network := cfg.Network
	if network == "" {
		network = r.Cfg.Network
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	for _, v := range cfg.Volumes {
		args = append(args, "-v", r.resolveVolume(v))
	}
	for _, e := range cfg.Env {
		args = append(args, "-e", e)
	}

	// Добавляем переменные окружения для GOPATH и PATH
	args = append(args, "-e", "GOPATH=/go")
	args = append(args, "-e", "GOROOT=/usr/local/go")
	// Важно: PATH должен быть таким же как в Dockerfile, но мы его пробрасываем явно при run
	args = append(args, "-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/go/bin:/root/go/bin")
	// Прокидываем PYTHONPATH на всякий случай
	args = append(args, "-e", "PYTHONPATH=/usr/lib/python3/dist-packages")

	args = append(args, cfg.Image)

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", containerName, err, string(out))
	}

	logger.Log().Info().Msg("docker: LSP container started successfully")
	return nil
}
