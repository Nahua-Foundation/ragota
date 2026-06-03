package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ragota/pkg/config"
	"ragota/pkg/docker"
	"ragota/pkg/qdrant"

	"github.com/spf13/cobra"
)

// newCleanCmd: ai-tools clean [directory]
//
// Полностью очищает индексы и базы данных, созданные ragota:
//   - удаляет коллекцию Qdrant (REST DELETE /collections/<name>)
//   - удаляет SQLite-базу tree-sitter (.ragota/treesitter.db)
//   - по флагу --all удаляет всю директорию .ragota (включая локальное хранилище qdrant)
//
// Без флагов работает мягко: только индекс-данные, без конфига и логов.
func newCleanCmd() *cobra.Command {
	var (
		all          bool
		skipQdrant   bool
		skipSQLite   bool
		alsoStorages bool
	)
	c := &cobra.Command{
		Use:   "clean [directory]",
		Short: "Drop Qdrant collection, SQLite DB and (optionally) all ai-tools data",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cfg, err := config.Load(dir, configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()

			var errs []error

			// 0. Проверяем, что контейнеры Docker не запущены.
			runner := docker.New(cfg.Root, cfg.Docker)
			ps, _ := runner.Ps(ctx)
			for _, svc := range ps {
				if svc.State == "running" {
					return fmt.Errorf("docker container %q is running — stop ai-tools (Ctrl+C) before cleaning", svc.Name)
				}
			}

			// 1. Qdrant collections (code + text + legacy).
			if !skipQdrant {
				qd := qdrant.New(fmt.Sprintf("http://%s:%d", cfg.Qdrant.Host, cfg.Qdrant.Port))
				// Быстрая проверка — если Qdrant недоступен, пропускаем
				pCtx, pCancel := context.WithTimeout(ctx, 1*time.Second)
				if err := qd.Ping(pCtx); err != nil {
					pCancel()
					fmt.Fprintf(os.Stderr, "qdrant: not reachable at %s:%d (skip collection cleanup)\n", cfg.Qdrant.Host, cfg.Qdrant.Port)
				} else {
					pCancel()
					names := map[string]bool{
						cfg.Collection:            true,
						cfg.CodeCollection().Name: true,
						cfg.TextCollection().Name: true,
					}
					delete(names, "")
					for name := range names {
						delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
						if err := qd.DeleteCollection(delCtx, name); err != nil {
							fmt.Fprintf(os.Stderr, "qdrant: %v (skipped %s)\n", err, name)
						} else {
							fmt.Fprintf(os.Stderr, "qdrant: collection %q deleted\n", name)
						}
						delCancel()
					}
				}
			}

			// 2. SQLite tree-sitter DB.
			if !skipSQLite {
				if err := removeIfExists(cfg.SQLitePath()); err != nil {
					errs = append(errs, fmt.Errorf("remove sqlite: %w", err))
				} else {
					fmt.Fprintf(os.Stderr, "sqlite: %s removed\n", cfg.SQLitePath())
				}
				_ = removeIfExists(cfg.SQLitePath() + "-wal")
				_ = removeIfExists(cfg.SQLitePath() + "-shm")
			}

			// 2b. BM25 (Bleve) index.
			if !skipSQLite {
				bm25Path := cfg.BM25Path()
				if err := os.RemoveAll(bm25Path); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("remove bm25: %w", err))
				} else {
					fmt.Fprintf(os.Stderr, "bm25: %s removed\n", bm25Path)
				}
			}

			// 3. Локальные docker-storage (qdrant bind-mounts).
			if alsoStorages || all {
				for _, name := range []string{"qdrant_storage"} {
					p := filepath.Join(cfg.DataDir(), name)
					if err := os.RemoveAll(p); err != nil {
						errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
					} else if _, statErr := os.Stat(p); os.IsNotExist(statErr) {
						fmt.Fprintf(os.Stderr, "storage: %s removed\n", p)
					}
				}
			}

			// 4. Полное удаление ai-tools.
			if all {
				if err := os.RemoveAll(cfg.DataDir()); err != nil {
					errs = append(errs, fmt.Errorf("remove ai-tools: %w", err))
				} else {
					fmt.Fprintf(os.Stderr, "data: %s removed\n", cfg.DataDir())
				}
			}

			if len(errs) > 0 {
				return errors.Join(errs...)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "remove the whole ai-tools directory (config, logs, storages)")
	c.Flags().BoolVar(&skipQdrant, "skip-qdrant", false, "don't delete the Qdrant collection")
	c.Flags().BoolVar(&skipSQLite, "skip-sqlite", false, "don't delete the tree-sitter SQLite DB")
	c.Flags().BoolVar(&alsoStorages, "storages", false, "also remove ai-tools/qdrant_storage")
	return c
}

// removeIfExists удаляет файл; отсутствие — не ошибка.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
