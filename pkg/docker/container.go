package docker

// Файл содержит запуск контейнеров: runContainer, runLSPContainer.

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"ragota/pkg/config"
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

// runContainer запускает контейнер: если уже существует — стартует, иначе docker run -di.
func (r *Runner) runContainer(ctx context.Context, c config.DockerContainerCfg) error {
	if c.Name == "" || c.Image == "" {
		return nil
	}
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	if st, err := r.inspectState(tCtx, c.Name); err == nil {
		if id, err := imageID(tCtx, c.Image); err == nil && st.Image != "" && st.Image != id {
			logger.Log().Warn().Str("container", c.Name).Msg("docker: outdated image, recreating")
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", c.Name).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			logger.Log().Warn().Str("container", c.Name).Str("status", st.Status).Msg("docker: bad state, recreating")
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", c.Name).Run()
		} else if r.volumesMismatch(st, c.Volumes) {
			logger.Log().Warn().Str("container", c.Name).Msg("docker: stale volumes, recreating")
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", c.Name).Run()
		} else {
			if st.Running {
				return nil
			}
			out, err := exec.CommandContext(tCtx, "docker", "start", c.Name).CombinedOutput()
			if err != nil {
				return fmt.Errorf("docker start %s: %w: %s", c.Name, err, string(out))
			}
			return nil
		}
	}
	args := buildRunArgs(c.Name, c.Network, c.GPU, c.Ports, c.Volumes, r)
	env := buildEnv(c.Env)
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
				if strings.Contains(v, "CERT") || strings.Contains(v, "BUNDLE") {
					if _, err := os.Stat(val); err == nil {
						args = append(args, "-v", val+":"+val+":ro")
						hostCertsMounted = true
					}
				}
			}
		}
	}
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
	out, err := exec.CommandContext(tCtx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", c.Name, err, string(out))
	}
	return nil
}

// runLSPContainer запускает единый LSP-контейнер.
func (r *Runner) runLSPContainer(ctx context.Context, cfg config.LSPDockerCfg) error {
	if cfg.Image == "" {
		return nil
	}
	containerName := "ragota-lsp"
	if err := r.ensureLSPImage(ctx, cfg.Image); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}
	tCtx, cancel := withCLI(ctx)
	defer cancel()
	if st, err := r.inspectState(tCtx, containerName); err == nil {
		if id, err := imageID(tCtx, cfg.Image); err == nil && st.Image != "" && st.Image != id {
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", containerName).Run()
		} else if st.Status == "restarting" || (!st.OpenStdin && st.Status != "running") {
			_ = exec.CommandContext(tCtx, "docker", "rm", "-f", containerName).Run()
		} else if r.volumesMismatch(st, cfg.Volumes) {
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
	args := buildRunArgs(containerName, cfg.Network, "", nil, cfg.Volumes, r)
	for _, e := range cfg.Env {
		args = append(args, "-e", e)
	}
	args = append(args, "-e", "GOPATH=/go", "-e", "GOROOT=/usr/local/go")
	args = append(args, "-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/go/bin:/root/go/bin")
	args = append(args, "-e", "PYTHONPATH=/usr/lib/python3/dist-packages")
	args = append(args, cfg.Image)
	out, err := exec.CommandContext(tCtx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s: %w: %s", containerName, err, string(out))
	}
	logger.Log().Info().Msg("docker: LSP container started")
	return nil
}

func buildRunArgs(name, network, gpu string, ports, volumes []string, r *Runner) []string {
	args := []string{"run", "-di", "--init", "--name", name, "--restart", "unless-stopped"}
	if network == "" {
		network = r.Cfg.Network
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if gpu != "" {
		args = append(args, "--gpus", gpu)
	}
	for _, p := range ports {
		args = append(args, "-p", p)
	}
	for _, v := range volumes {
		args = append(args, "-v", r.resolveVolume(v))
	}
	return args
}

func buildEnv(cfgEnv []string) []string {
	return append([]string{}, cfgEnv...)
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
