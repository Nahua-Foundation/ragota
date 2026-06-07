package cli

// docker_bootstrap.go — мониторинг Docker-контейнеров для TUI.

import (
	"context"
	"fmt"
	"time"

	"ragota/pkg/docker"
	"ragota/pkg/state"
)

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
