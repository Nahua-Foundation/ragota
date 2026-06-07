package cli

// cli_clean.go — ragota clean [dir] и ragota clean cross-repo [dir].

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"ragota/internal/store"
	"ragota/pkg/config"
	"ragota/pkg/docker"

	"github.com/spf13/cobra"
)

func newCleanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "clean [directory]",
		Short: "Drop all indexes and databases (Qdrant, SQLite, BM25)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			// Преобразуем в абсолютный путь ДО загрузки конфига
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			cfg, err := config.Load(absDir)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()

			var errs []error

			// Проверяем, что контейнеры Docker не запущены
			runner := docker.New(cfg.Root)
			ps, _ := runner.Ps(ctx)
			for _, svc := range ps {
				if svc.State == "running" {
					return fmt.Errorf("docker container %q is running — stop ragota (Ctrl+C) before cleaning", svc.Name)
				}
			}

			// Qdrant collections — удаляем volume напрямую (без запуска контейнера)
			fmt.Fprintf(os.Stderr, "qdrant: removing storage volume...\n")
			stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
			// Останавливаем контейнер если запущен
			_ = exec.CommandContext(stopCtx, "docker", "stop", "-t", "3", "ragota-qdrant").Run()
			stopCancel()
			
			// Удаляем volume
			rmCtx, rmCancel := context.WithTimeout(ctx, 10*time.Second)
			out, err := exec.CommandContext(rmCtx, "docker", "volume", "rm", "-f", "ragota-qdrant-storage").CombinedOutput()
			rmCancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "qdrant: volume remove: %v: %s (skip collection cleanup)\n", err, string(out))
			} else {
				fmt.Fprintf(os.Stderr, "qdrant: volume removed\n")
			}

			// SQLite + WAL/SHM
			if err := removeFile(cfg.SQLitePath()); err != nil {
				errs = append(errs, fmt.Errorf("remove sqlite: %w", err))
			} else {
				fmt.Fprintf(os.Stderr, "sqlite: %s removed\n", cfg.SQLitePath())
			}
			_ = removeFile(cfg.SQLitePath() + "-wal")
			_ = removeFile(cfg.SQLitePath() + "-shm")

			// BM25
			bm25Path := cfg.BM25Path()
			if err := os.RemoveAll(bm25Path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove bm25: %w", err))
			} else {
				fmt.Fprintf(os.Stderr, "bm25: %s removed\n", bm25Path)
			}

			if len(errs) > 0 {
				return errors.Join(errs...)
			}
			return nil
		},
	}

	// Подкоманда cross-repo
	c.AddCommand(&cobra.Command{
		Use:   "cross-repo [directory]",
		Short: "Delete only cross-repo edges and reset cross_hash",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			st, err := store.Open(cfg.SQLitePath())
			if err != nil {
				return fmt.Errorf("open sqlite: %w", err)
			}
			defer st.Close()

			ctx := cmd.Context()
			var errs []error

			if err := st.DeleteCrossCallEdges(ctx); err != nil {
				errs = append(errs, fmt.Errorf("delete crossrepo edges: %w", err))
			} else {
				fmt.Fprintf(os.Stderr, "crossrepo: cross_call edges deleted\n")
			}
			if err := st.ResetCrossHashes(ctx); err != nil {
				errs = append(errs, fmt.Errorf("reset cross_hash: %w", err))
			} else {
				fmt.Fprintf(os.Stderr, "crossrepo: cross_hash reset\n")
			}

			if len(errs) > 0 {
				return errors.Join(errs...)
			}
			return nil
		},
	})

	return c
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
