package config

// Файл реализует загрузку и сохранение конфига:
// Load — чтение YAML с дефолтами из ~/.ragota/config.yaml + загрузка .ragotaignore;
// WriteDefault — запись дефолтного конфига;
// EnsureDataDir — создание служебных каталогов .ragota/ и logs/;
// HomeConfigPath — возвращает ~/.ragota/config.yaml.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ragota/pkg/ragignore"

	yaml "gopkg.in/yaml.v3"
)

// HomeConfigPath возвращает глобальный путь конфига: ~/.ragota/config.yaml.
func HomeConfigPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".ragota", "config.yaml")
}

// Load загружает конфиг из ~/.ragota/config.yaml и .ragotaignore из root.
// Если файла конфига нет — возвращаются дефолтные значения.
// .ragotaignore загружается всегда (или DefaultPatterns если файла нет).
func Load(root string) (*Config, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	cfg := Default()
	cfg.Root = absRoot

	path := HomeConfigPath()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Root = absRoot

	// Загружаем .ragotaignore из root
	patterns, perr := ragignore.Load(absRoot)
	if perr != nil {
		// Не ошибка — логируем warning, используем DefaultPatterns
		fmt.Fprintf(os.Stderr, "ragota: warning: loading .ragotaignore: %v\n", perr)
		patterns = ragignore.DefaultPatterns
	}
	cfg.IgnorePatterns = patterns

	return cfg, nil
}

// WriteDefault записывает дефолтный конфиг в указанный путь.
func WriteDefault(path string, overwrite bool) (string, error) {
	if _, err := os.Stat(path); err == nil && !overwrite {
		return path, fmt.Errorf("config already exists: %s (use --force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}
	cfg := Default()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return path, err
	}
	header := []byte("# ragota default configuration.\n" +
		"# Place this file at ~/.ragota/config.yaml.\n\n")
	if err := os.WriteFile(path, append(header, data...), 0o644); err != nil {
		return path, err
	}
	return path, nil
}

// EnsureDataDir создаёт .ragota/ и .ragota/logs/ в корне проекта.
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir(), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(c.DataDir(), "logs"), 0o755)
}
