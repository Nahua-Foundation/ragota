package docker

// Файл содержит запуск контейнеров: runQdrant, runLSPContainer.

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"ragota/pkg/logger"
)

//go:embed Dockerfile.lsp
var lspDockerfile []byte

var proxyVars = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
}

// runQdrant запускает Qdrant контейнер.
func (r *Runner) runQdrant(ctx context.Context, network string) error {
	name := "ragota-qdrant"
	image := "qdrant/qdrant:latest"

	tCtx, cancel := withCLI(ctx)
	defer cancel()

	// Проверяем существующий контейнер
	if st, err := r.inspectState(tCtx, name); err == nil {
		if id, err := imageID(tCtx, image); err == nil && st.Image != "" && st.Image != id {
			logger.Log().Warn().Str("container", name).Msg("docker: outdated image, recreating")
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", name).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			logger.Log().Warn().Str("container", name).Str("status", st.Status).Msg("docker: bad state, recreating")
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", name).Run()
		} else {
			// Контейнер существует и в хорошем состоянии — читаем его порт
			port, err := r.getExistingPort(name)
			if err != nil {
				// Не удалось прочитать порт — пересоздаём контейнер
				logger.Log().Warn().Str("container", name).Err(err).Msg("docker: cannot read port, recreating")
				_ = exec.CommandContext(tCtx, "docker", "rm", "-f", name).Run()
			} else {
				r.QdrantPort = port
				logger.Log().Info().Int("port", port).Msg("docker: reusing existing qdrant port")
				if st.Running {
					return nil
				}
				out, err := exec.CommandContext(tCtx, "docker", "start", name).CombinedOutput()
				if err != nil {
					return fmt.Errorf("docker start %s: %w: %s", name, err, string(out))
				}
				return nil
			}
		}
	}

	// Контейнера нет или был удалён/пересоздан — генерируем новый порт
	port, err := findFreePort()
	if err != nil {
		return fmt.Errorf("find free port: %w", err)
	}
	r.QdrantPort = port
	logger.Log().Info().Int("port", port).Msg("docker: starting qdrant with new port")

	portMapping := fmt.Sprintf("127.0.0.1:%d:6333", port)
	args := []string{"run", "-di", "--init", "--name", name, "--restart", "unless-stopped"}
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args, "-p", portMapping)
	args = append(args, "-v", r.resolveVolume(".ragota/qdrant_storage:/qdrant/storage"))
	args = append(args, image)

	out, err := exec.CommandContext(tCtx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", name, err, string(out))
	}
	return nil
}

// runLSPContainer запускает единый LSP-контейнер.
func (r *Runner) runLSPContainer(ctx context.Context, network string) error {
	image := "ragota-lsp:latest"
	containerName := "ragota-lsp"

	tCtx, cancel := withCLI(ctx)
	defer cancel()

	// Билдим образ если нет
	if err := r.ensureLSPImage(tCtx, image); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}

	if st, err := r.inspectState(tCtx, containerName); err == nil {
		if id, err := imageID(tCtx, image); err == nil && st.Image != "" && st.Image != id {
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", containerName).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", containerName).Run()
		} else {
			if st.Running {
				return nil
			}
			out, err := exec.CommandContext(tCtx, "docker", "start", containerName).CombinedOutput()
			if err != nil {
				return fmt.Errorf("docker start %s: %w: %s", containerName, err, string(out))
			}
			return nil
		}
	}

	args := []string{"run", "-di", "--init", "--name", containerName, "--restart", "unless-stopped"}
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args, "-v", r.WorkingDir+":/workspace")
	args = append(args, "-e", "GOPATH=/go", "-e", "GOROOT=/usr/local/go")
	args = append(args, "-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/go/bin:/root/go/bin")
	args = append(args, "-e", "PYTHONPATH=/usr/lib/python3/dist-packages")

	// Proxy vars from host
	for _, v := range proxyVars {
		if val := os.Getenv(v); val != "" {
			args = append(args, "-e", v+"="+val)
		}
	}
	if runtime.GOOS == "darwin" {
		if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
			macCert := "/etc/ssl/cert.pem"
			if _, err := os.Stat(macCert); err == nil {
				args = append(args, "-v", macCert+":"+macCert+":ro")
				args = append(args, "-e", "SSL_CERT_FILE="+macCert)
			}
		}
	}

	args = append(args, image)

	out, err := exec.CommandContext(tCtx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", containerName, err, string(out))
	}
	logger.Log().Info().Msg("docker: LSP container started")
	return nil
}

func (r *Runner) ensureLSPImage(ctx context.Context, image string) error {
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	if err := exec.CommandContext(tCtx, "docker", "image", "inspect", image).Run(); err == nil {
		return nil
	}
	logger.Log().Info().Str("image", image).Msg("docker: building LSP image")
	bCtx, bCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer bCancel()
	cmd := exec.CommandContext(bCtx, "docker", "build", "-t", image, "-")
	cmd.Stdin = strings.NewReader(string(lspDockerfile))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *Runner) volumesMismatch(st *containerState, volumes []string) bool {
	if len(st.Mounts) == 0 || len(volumes) == 0 {
		return false
	}
	existing := make(map[string]bool)
	for _, m := range st.Mounts {
		existing[m] = true
	}
	for _, v := range volumes {
		if !existing[r.resolveVolume(v)] {
			return true
		}
	}
	return false
}
