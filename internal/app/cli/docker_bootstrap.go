package cli

// Файл содержит общую логику запуска Docker-контейнеров,
// используемую и cli_run, и cli_watch.

import (
	"context"
	"fmt"
	"os"
	"time"

	"ragota/pkg/config"
	"ragota/pkg/docker"
	"ragota/pkg/state"
)

// bootstrapDocker запускает Docker-контейнеры и ждёт готовности Qdrant.
// Возвращает runner для дальнейшего использования (мониторинг, остановка).
func bootstrapDocker(ctx context.Context, cfg *config.Config, bus *state.Bus, startAll bool) (*docker.Runner, error) {
	runner := docker.New(cfg.Root, cfg.Docker)

	if err := docker.Available(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "docker: check failed: %v\n", err)
		bus.SetDocker(state.DockerStatus{LastError: err.Error()})
		if startAll {
			return nil, fmt.Errorf("docker required but unavailable: %w", err)
		}
		return runner, nil
	}

	fmt.Fprintf(os.Stderr, "docker: starting containers...\n")
	bus.SetDocker(state.DockerStatus{LastError: "starting..."})
	if err := runner.Up(ctx, startAll); err != nil {
		fmt.Fprintf(os.Stderr, "docker: error starting containers: %v\n", err)
		bus.SetDocker(state.DockerStatus{LastError: err.Error()})
		if startAll {
			return nil, fmt.Errorf("docker containers failed to start: %w", err)
		}
		return runner, nil
	}

	fmt.Fprintf(os.Stderr, "docker: waiting for Qdrant to become ready...\n")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := runner.WaitQdrant(waitCtx); err != nil {
		fmt.Fprintf(os.Stderr, "docker: Qdrant not ready: %v (continuing anyway)\n", err)
	}
	waitCancel()
	fmt.Fprintf(os.Stderr, "docker: all containers are up\n")
	go startDockerMonitor(ctx, runner, bus)

	return runner, nil
}

// startDockerMonitor периодически обновляет статус контейнеров в bus для TUI.
func startDockerMonitor(ctx context.Context, r *docker.Runner, bus *state.Bus) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ps, err := r.Ps(ctx)
			s := state.DockerStatus{Running: true}
			if err != nil {
				s.LastError = err.Error()
			} else {
				for _, p := range ps {
					s.Services = append(s.Services, fmt.Sprintf("%s(%s)", p.Name, p.State))
				}
			}
			bus.SetDocker(s)
		}
	}
}
