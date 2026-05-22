package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"aitools/internal/config"
	"aitools/internal/docker"
	"aitools/internal/lsp"

	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Check and install dependencies (docker, ollama, models, lsp)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			scanner := bufio.NewScanner(os.Stdin)

			cfg, err := config.Load(".", configPath)
			if err != nil {
				cfg = config.Default()
			}

			fmt.Println("=== Checking dependencies ===")

			// 1. Docker
			if err := docker.Available(ctx); err != nil {
				fmt.Printf("[ ] Docker is not available: %v\n", err)
				if askConfirm(scanner, "Install Docker?") {
					if err := installDocker(ctx); err != nil {
						fmt.Printf("Failed to install Docker: %v\n", err)
					} else {
						fmt.Println("[+] Docker installed successfully (you might need to start it manually)")
					}
				}
			} else {
				fmt.Println("[v] Docker is installed")
			}

			// 2. Ollama
			if !isCommandAvailable("ollama") {
				fmt.Println("[ ] Ollama is not installed")
				if askConfirm(scanner, "Install Ollama?") {
					if err := installOllama(ctx); err != nil {
						fmt.Printf("Failed to install Ollama: %v\n", err)
					} else {
						fmt.Println("[+] Ollama installed successfully")
					}
				}
			} else {
				fmt.Println("[v] Ollama is installed")
			}

			// 3. nomic-embed-text (if ollama is available)
			if isCommandAvailable("ollama") {
				modelName := cfg.Ollama.EmbedModel
				if modelName == "" {
					modelName = "nomic-embed-text"
				}
				if !isOllamaModelAvailable(ctx, modelName) {
					fmt.Printf("[ ] %s model is not available\n", modelName)
					if askConfirm(scanner, fmt.Sprintf("Pull %s model?", modelName)) {
						if err := pullOllamaModel(ctx, modelName); err != nil {
							fmt.Printf("Failed to pull model: %v\n", err)
						} else {
							fmt.Printf("[+] %s model pulled successfully\n", modelName)
						}
					}
				} else {
					fmt.Printf("[v] %s model is available\n", modelName)
				}
			}

			// 4. LSP Servers
			fmt.Println("\n--- LSP Servers ---")
			specs := lsp.DefaultServers()
			for _, spec := range specs {
				if _, err := exec.LookPath(spec.Command); err != nil {
					fmt.Printf("[ ] %s (%s) is not installed\n", spec.Language, spec.Command)
					if askConfirm(scanner, fmt.Sprintf("Install %s?", spec.Command)) {
						if err := installLSP(ctx, spec); err != nil {
							fmt.Printf("Failed to install %s: %v\n", spec.Command, err)
						} else {
							fmt.Printf("[+] %s installed successfully\n", spec.Command)
						}
					}
				} else {
					fmt.Printf("[v] %s (%s) is installed\n", spec.Language, spec.Command)
				}
			}

			fmt.Println("\n=== Installation check finished ===")
			return nil
		},
	}
}

func askConfirm(scanner *bufio.Scanner, question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	if !scanner.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes"
}

func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isOllamaModelAvailable(ctx context.Context, modelName string) bool {
	out, err := exec.CommandContext(ctx, "ollama", "list").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), modelName)
}

func pullOllamaModel(ctx context.Context, modelName string) error {
	fmt.Printf("Pulling %s...\n", modelName)
	cmd := exec.CommandContext(ctx, "ollama", "pull", modelName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installDocker(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		if isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "--cask", "docker")
		}
		return fmt.Errorf("homebrew not found, please install Docker Desktop manually from https://www.docker.com/products/docker-desktop/")
	case "linux":
		return runCmd(ctx, "sh", "-c", "curl -fsSL https://get.docker.com | sh")
	default:
		return fmt.Errorf("unsupported OS for automatic docker installation: %s", runtime.GOOS)
	}
}

func installOllama(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		if isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "--cask", "ollama")
		}
		return fmt.Errorf("homebrew not found, please install Ollama manually from https://ollama.com")
	case "linux":
		return runCmd(ctx, "sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	default:
		return fmt.Errorf("unsupported OS for automatic ollama installation: %s", runtime.GOOS)
	}
}

func installLSP(ctx context.Context, spec lsp.ServerSpec) error {
	switch spec.Command {
	case "gopls":
		return runCmd(ctx, "go", "install", "golang.org/x/tools/gopls@latest")
	case "typescript-language-server":
		return runCmd(ctx, "npm", "install", "-g", "typescript-language-server", "typescript")
	case "pyright-langserver":
		return runCmd(ctx, "npm", "install", "-g", "pyright")
	case "jdtls":
		if runtime.GOOS == "darwin" && isCommandAvailable("brew") {
			return runCmd(ctx, "brew", "install", "jdtls")
		}
		return fmt.Errorf("automatic installation of jdtls is not supported on this OS or without homebrew")
	default:
		return fmt.Errorf("unknown LSP server: %s", spec.Command)
	}
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
