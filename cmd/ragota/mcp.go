package main

// The mcp subcommand serves a running ragota to a coding agent over MCP.
//
// It speaks MCP on stdin/stdout, which is what an MCP client launching a
// subprocess expects, so nothing but protocol may be written to stdout: every
// diagnostic goes to stderr, where the client surfaces it. It dispatches in
// main before the server's config file is even looked for, because it needs
// nothing from that file — its configuration is the environment of the launch
// block that started it, which is the one place an MCP client lets a user put
// settings.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Nahua-Foundation/ragota/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Nahua-Foundation/ragota/internal/mcp/config"
	"github.com/Nahua-Foundation/ragota/internal/mcp/server"
)

func runMCP(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("ragota mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		url         = fs.String("url", "", "ragota base URL (env RAGOTA_URL)")
		timeout     = fs.Duration("timeout", 0, "budget for one whole tool call (env RAGOTA_TIMEOUT)")
		repos       = fs.String("repos", "", "comma-separated default repository scope (env RAGOTA_REPOS)")
		maxBytes    = fs.Int("max-bytes", 0, "default response budget in bytes (env RAGOTA_MAX_BYTES)")
		authStyle   = fs.String("auth-style", "", "api-key or bearer (env RAGOTA_AUTH_STYLE)")
		checkOnly   = fs.Bool("check", false, "verify the connection to ragota and exit")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `ragota mcp %s — MCP server over a running ragota.

Usage: ragota mcp [flags]

Configuration comes from the environment, which is what an MCP client's launch
block sets; these flags override it for testing by hand. The API key is
environment-only, because a flag would put it in the process table.

  RAGOTA_URL          ragota base URL (default %s)
  RAGOTA_MCP_KEY      API key; a read-scoped one is enough and is the point
  RAGOTA_API_KEY      fallback when RAGOTA_MCP_KEY is unset
  RAGOTA_AUTH_STYLE   %s (default) or %s
  RAGOTA_TIMEOUT      budget for one whole tool call (default %s)
  RAGOTA_MAX_BYTES    default response budget in bytes (default %d)
  RAGOTA_REPOS        comma-separated default repository scope

Flags:
`, version, config.DefaultBaseURL, config.AuthAPIKey, config.AuthBearer, config.DefaultTimeout, config.DefaultMaxBytes)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stderr, version)
		return nil
	}

	cfg, err := config.FromEnv(config.OSEnv)
	if err != nil {
		return err
	}
	overrideString(&cfg.BaseURL, *url)
	overrideString(&cfg.AuthStyle, strings.ToLower(*authStyle))
	if *timeout > 0 {
		cfg.Timeout = *timeout
	}
	if *maxBytes > 0 {
		cfg.MaxBytes = *maxBytes
	}
	if *repos != "" {
		cfg.Repos = strings.FieldsFunc(*repos, func(r rune) bool { return r == ',' })
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	s := server.New(cfg, mcpClient(cfg))

	// The startup check runs before the transport is connected, so a
	// misconfiguration is a process that refuses to start with a reason on
	// stderr — which the client surfaces — rather than ten identical tool
	// failures inside a model's turn.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checkCtx, cancel := context.WithTimeout(ctx, startupTimeout(cfg.Timeout))
	health, err := s.StartupCheck(checkCtx)
	cancel()
	if err != nil {
		return err
	}

	if *checkOnly {
		_, _ = fmt.Fprintf(stderr, "ok: ragota %s (api %s) at %s, %d tools\n",
			health.Version, health.APIVersion, cfg.BaseURL, len(server.ToolNames()))
		return nil
	}

	_, _ = fmt.Fprintf(stderr, "ragota mcp %s serving %d tools over stdio: %s (ragota %s, api %s)\n",
		version, len(server.ToolNames()), cfg.Redacted(), health.Version, health.APIVersion)
	return s.MCP(version).Run(ctx, &mcp.StdioTransport{})
}

// mcpClient builds the ragota client the tools call.
//
// What actually bounds a tool call is the per-call deadline each handler sets,
// because that one covers the client's retries as well; the transport timeout
// here only bounds a single attempt, and is set alongside it so that a
// connection which never returns cannot outlive the deadline by a whole attempt.
func mcpClient(cfg *config.Config) *client.Client {
	opts := []client.Option{
		client.WithUserAgent("ragota/" + version + " (mcp)"),
		client.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
	}
	if cfg.APIKey != "" {
		if cfg.AuthStyle == config.AuthBearer {
			opts = append(opts, client.WithBearerToken(cfg.APIKey))
		} else {
			opts = append(opts, client.WithAPIKey(cfg.APIKey))
		}
	}
	return client.New(cfg.BaseURL, opts...)
}

// startupTimeout bounds the checks. It is deliberately shorter than a tool
// call's budget: a client waiting to launch a server has less patience than a
// model waiting for an answer, and an unreachable address should say so quickly.
func startupTimeout(callTimeout time.Duration) time.Duration {
	const limit = 15 * time.Second
	return min(callTimeout, limit)
}

func overrideString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
