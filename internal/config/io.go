package config

// Файл реализует загрузку и сохранение конфига:
// ResolveConfigPath/DefaultConfigPath/HomeConfigPath — алгоритм поиска;
// Load — чтение YAML с дефолтами; WriteDefault — запись дефолтного конфига;
// EnsureDataDir — создание служебных каталогов .ragota/ и logs/.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yaml "gopkg.in/yaml.v3"
)

// DefaultConfigPath возвращает локальный путь конфига: .ragota/config.yaml в корне.
func DefaultConfigPath(root string) string {
	return filepath.Join(root, ".ragota", "config.yaml")
}

// HomeConfigPath возвращает глобальный путь конфига: ~/.ragota/config.yaml.
func HomeConfigPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".ragota", "config.yaml")
}

// ResolveConfigPath возвращает путь к конфигу, который будет загружен.
func ResolveConfigPath(root, configPath string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := configPath
	if path == "" {
		// Сначала ищем локальный (.ragota/config.yaml или старый ai-tools/config.yaml)
		local := DefaultConfigPath(absRoot)
		if _, err := os.Stat(local); err == nil {
			path = local
		} else {
			oldLocal := filepath.Join(absRoot, "ragota", "config.yaml")
			if info, err := os.Stat(oldLocal); err == nil && !info.IsDir() {
				path = oldLocal
			} else {
				global := HomeConfigPath()
				if global != "" {
					if _, err := os.Stat(global); err == nil {
						path = global
					} else {
						path = global
					}
				} else {
					path = local
				}
			}
		}
	}
	return filepath.Abs(path)
}

// Load загружает конфиг. Порядок поиска если configPath пустой:
// 1. .ragota/config.yaml (локальный в корне проекта)
// 2. ~/.ragota/config.yaml (глобальный в HOME)
// Если файла нет нигде — возвращается дефолт.
func Load(root, configPath string) (*Config, error) {
	path, err := ResolveConfigPath(root, configPath)
	if err != nil {
		return nil, err
	}
	absRoot, _ := filepath.Abs(root)
	cfg := Default()
	cfg.Root = absRoot

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if configPath != "" {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		// Файла нет — возвращаем дефолты.
		return cfg, nil
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Root = absRoot
	return cfg, nil
}

// WriteDefault записывает дефолтный конфиг в указанный путь.
// Если path пустой — пишет в HomeConfigPath().
// Если файл уже существует и overwrite=false — возвращается ошибка.
func WriteDefault(path string, overwrite bool) (string, error) {
	if path == "" {
		path = HomeConfigPath()
		if path == "" {
			return "", fmt.Errorf("could not determine home directory for config")
		}
	}
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
		"# Place this file at .ragota/config.yaml or pass via --config <path>.\n" +
		"# If you are behind a corporate proxy, add HTTP_PROXY/HTTPS_PROXY to docker envs.\n\n")
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
