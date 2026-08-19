package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/app"
	"github.com/Nahua-Foundation/ragota/internal/config"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/server/api"
	"github.com/Nahua-Foundation/ragota/internal/server/bootstrap"
	"github.com/Nahua-Foundation/ragota/internal/server/progress"
	"github.com/Nahua-Foundation/ragota/internal/server/tui"
	"github.com/Nahua-Foundation/ragota/internal/server/watch"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/ragota
var version = "dev"

// Exit codes. 2 distinguishes "the config is valid but something it points at
// is unreachable" from "the config is wrong", so CI can treat them apart.
//
// exitUsage shares the value 2 with exitDepUnreachable, and deliberately: it is
// what stdlib flag exits with on an unknown flag, so an unknown flag and an
// unknown subcommand look the same to whatever ran the binary. The two never
// collide in practice — exitDepUnreachable comes only from --check-config,
// which is a mode the process cannot reach before its arguments have parsed.
//
// exitFailure shares the value 1 with exitConfigInvalid on the same reasoning:
// a repos subcommand that could not do its job — the store would not open, the
// repository it named is not registered — has failed the invocation, and no
// caller has to tell that apart from an unusable config, since one process is
// never both.
const (
	exitConfigInvalid  = 1
	exitFailure        = 1
	exitDepUnreachable = 2
	exitUsage          = 2
)

// The subcommands. commandRun starts the server and is optional: every
// invocation that predates it — `ragota --config config.yaml` — has no
// subcommand at all and has to keep working, so an empty argument list means
// run. commandRepos reads and edits the index's composition and exits.
//
// No dispatch layer and no CLI framework. Stdlib flag stops at the first
// non-flag argument, so `--source ./dir run` leaves the positional arguments
// behind and flags-then-subcommand parses on its own. What is worth spending
// code on is the other half: an unrecognized word is rejected rather than
// ignored, because silently starting the server for `ragota --source .
// rnu` is how a typo becomes a bug report about a flag that "did nothing".
const (
	commandRun    = "run"
	commandRepos  = "repos"
	commandMCP    = "mcp"
	commandInit   = "init"
	commandSkills = "skills"
)

// fatalf reports a fatal error directly on stderr and exits. Directly, not
// through slog, and that is the point: once a dashboard is requested the log
// handler points at the status bus and the interactive log file, so a process
// that dies before the dashboard draws — a bad config, an unreachable
// dependency, a taken port — would take its last words with it. That was the
// observed failure: `--interactive run` over a broken wiring exited to a
// clean prompt with nothing printed at all. os.Exit skips deferred cleanup
// exactly as the log.Fatalf it replaces did.
func fatalf(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "ragota: "+format+"\n", v...)
	os.Exit(exitFailure)
}

func main() {
	var (
		cfgPath     = flag.String("config", "", "path to the YAML config file (overrides RAGOTA_CONFIG; default "+config.DefaultConfigPath+")")
		showVersion = flag.Bool("version", false, "print version information and exit")
		checkConfig = flag.Bool("check-config", false, "load and validate the config, probe every configured dependency, and exit without touching the database")
		logLevel    = flag.String("log-level", "", "override log.level (debug|info|warn|error)")
		pprofAddr   = flag.String("pprof", "", "serve net/http/pprof on this host:port (e.g. 127.0.0.1:6060); empty disables profiling")
		source      = flag.String("source", "", "directory holding the repositories to index; they are discovered, registered and indexed on startup")
		watchFS     = flag.Bool("watch", false, "keep the index in step with changes under the indexed repositories")
		interactive = flag.Bool("interactive", false, "show the indexing status as a terminal dashboard; the process log goes to a file while it is on screen (needs a terminal on stdout)")
	)
	flag.Usage = usage
	flag.Parse()

	cmd, err := parseCommand(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ragota: %v\n\n", err)
		flag.Usage()
		os.Exit(exitUsage)
	}

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	// The MCP subcommand configures itself from the environment of the launch
	// block and speaks protocol on stdout, so it dispatches before the config
	// file is loaded or a single line can be printed: the server's own config
	// has nothing it needs, and stdout belongs to the protocol from the first
	// byte.
	if cmd.name == commandMCP {
		if err := runMCP(cmd.mcp, os.Stderr); err != nil {
			fatalf("mcp: %v", err)
		}
		return
	}

	// The scaffolding subcommands write embedded files and exit — no config
	// load, no database, no log setup. `init` honours --config (and
	// RAGOTA_CONFIG) as its default target, because the file it writes is the
	// file those would read.
	if cmd.name == commandInit {
		path := cmd.initPath
		if path == "" {
			path = resolveConfigPath(*cfgPath)
		}
		os.Exit(runInit(path))
	}
	if cmd.name == commandSkills {
		dir := cmd.skillsDir
		if dir == "" {
			dir = defaultSkillsDir
		}
		os.Exit(runSkillsInstall(dir))
	}

	// The built-in profile stands in for a missing config file in the two
	// invocations a user reaches without ever writing one: a --source run, and
	// a repos subcommand asked about what that run left behind.
	cfg, err := loadConfig(*cfgPath, *source != "" || cmd.name == commandRepos)
	if err != nil {
		fatalf("failed to load config: %v", err)
	}
	if *source != "" {
		if _, err := bootstrap.ApplySource(cfg, *source); err != nil {
			fatalf("invalid --source: %v", err)
		}
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}

	// Whether the dashboard runs has to be settled before the log handler is
	// built, because it decides where the log goes: records on stderr and a
	// full-screen renderer on the same terminal interleave into garbage.
	//
	// A stdout that is not a terminal — a pipe, a redirect, a CI job — falls
	// back to the ordinary run instead of failing, since --interactive in a
	// script is a request that cannot be honoured rather than a mistake worth
	// exiting over. Neither --check-config nor a repos subcommand ever draws:
	// they print a report on the terminal the dashboard would have taken over.
	dashboard := *interactive && cmd.name == commandRun && !*checkConfig && tui.IsTerminal(os.Stdout)
	logs := newLogSink()
	var logFile *os.File
	var logPath string
	if dashboard {
		logFile, logPath = openInteractiveLog()
		logs.toFile(logFile)
		if logFile != nil {
			// Closed last of everything deferred here: it has to outlive every
			// component that still logs on its way down.
			defer func() { _ = logFile.Close() }()
		}
	}

	// Signal handling is established before anything that can consume a file
	// descriptor, because os/signal.Notify allocates a pipe and a pipe it
	// cannot allocate is `fatal error: pipe failed` — a runtime throw, not an
	// error this code could report. Registering it after the watcher meant
	// that exhausting descriptors killed the process at exactly the call that
	// would have let the user interrupt it.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// A bus exists only in the modes an interactive front end attaches to. The
	// server proper runs without one, which is why every publisher treats a nil
	// bus as a no-op rather than requiring one to be constructed.
	var bus *progress.Bus
	if *source != "" || *watchFS || dashboard {
		bus = progress.NewBus(0)
	}
	slog.SetDefault(slog.New(progress.NewLogHandler(newHandler(&cfg.Log, logs), bus, slog.LevelWarn)))
	if *interactive && !dashboard && !*checkConfig {
		slog.Warn("interactive: stdout is not a terminal; continuing without the dashboard")
	}
	if dashboard && logFile == nil {
		slog.Warn("interactive: no log file; entries below warn are dropped while the dashboard is on screen")
	}

	if *checkConfig {
		os.Exit(runCheckConfig(cfg, resolveConfigPath(*cfgPath)))
	}

	if err := cfg.Validate(); err != nil {
		fatalf("invalid config: %v", err)
	}
	for _, w := range cfg.Warnings() {
		slog.Warn("config", "warning", w)
	}

	// A repos subcommand opens the store, answers one question about it and
	// exits: no listener, no watcher, no source scan, and above all no working
	// set redefined behind the user's back — `repos list` is what one asks when
	// one is not sure what the last run did.
	if cmd.name == commandRepos {
		if *source != "" {
			// Said rather than quietly ignored: computing a working set out of
			// a directory is something only a run does, and a user who typed
			// --source here is asking for what `repos activate` does instead.
			slog.Warn("repos: --source changes the working set only in a run; this invocation leaves it alone")
		}
		os.Exit(runRepos(cfg, cmd.repos))
	}

	shutdownPprof := startPprof(*pprofAddr)

	ctx := context.Background()
	svc, err := bootstrap.Build(ctx, cfg)
	if err != nil {
		fatalf("failed to initialize: %v", err)
	}
	defer func() { _ = svc.Close(ctx) }()
	svc.SetStatusBus(bus)

	// The source scan and the watcher outlive individual requests but must not
	// outlive the storage they write to. They run under a context of their own,
	// stopped by a defer registered after svc.Close's — LIFO runs it first, so
	// the store is still open when the last of their writes lands.
	bgCtx, stopBackground := context.WithCancel(ctx)
	var bg sync.WaitGroup
	defer func() {
		stopBackground()
		bg.Wait()
	}()

	var registered []*domain.Repo
	if *source != "" {
		registered, err = bootstrap.DiscoverAndRegister(ctx, svc, *source, bus)
		if err != nil {
			fatalf("failed to scan --source: %v", err)
		}
		if len(registered) == 0 {
			slog.Warn("source: nothing registered; leaving the working set as it was", "source", *source)
		}
	}
	if err := activateSource(ctx, svc, *source, registered); err != nil {
		// Fatal, unlike most startup trouble: a run whose working set stayed
		// as the last one left it answers questions about this project out of
		// another project's code, silently. That confusion is what --source
		// computing the set exists to end, so failing to compute it ends the
		// run instead.
		fatalf("failed to set the working set: %v", err)
	}

	// Primed after the source scan, never before: priming reads the working set
	// out of the store, and until the scan has replaced it that is the previous
	// run's — the very set the dashboard must stop showing.
	if dashboard {
		primeStatusBus(ctx, svc, bus)
	}

	if len(registered) > 0 {
		bg.Add(1)
		go func() {
			defer bg.Done()
			// The repositories the source found are the working set, because
			// activateSource just made them it. Indexing them is indexing the
			// active set; there is no second list to read back.
			bootstrap.IndexAll(bgCtx, svc, registered)
		}()
	}

	server := api.NewServer(svc, &cfg.Server, api.WithVersion(version))

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      server.Router(),
		ReadTimeout:  seconds(cfg.Server.ReadTimeoutSeconds),
		WriteTimeout: seconds(cfg.Server.WriteTimeoutSeconds),
		IdleTimeout:  seconds(cfg.Server.IdleTimeoutSeconds),
	}

	// The socket is claimed before the watcher runs, not after. On a kqueue
	// platform a watch costs a descriptor per directory entry, so a large tree
	// can consume the process's whole allowance — and a listener that cannot be
	// opened is a server that answers nothing, which is a worse outcome than a
	// tree that is followed only in part. Holding the listener first makes that
	// trade impossible to get wrong: the API is up, and the watcher spends what
	// is left. (Measured on the benchmark corpus: without this, "listen tcp
	// 127.0.0.1:8080: socket: too many open files".)
	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		fatalf("failed to listen on %s: %v", addr, lerr)
	}

	if *watchFS {
		if stop := startWatcher(bgCtx, &bg, svc, bus); stop != nil {
			defer stop()
		}
	}

	// The listener's failure is reported rather than fatal here, so that the
	// one caller who has to act on it first — the dashboard, which is holding
	// the terminal — gets the chance. It ends the same way it always did.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", addr, "version", version,
			"write_timeout", srv.WriteTimeout, "read_timeout", srv.ReadTimeout)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	var fatal error
	if dashboard {
		// Quitting the dashboard leaves through the same door SIGTERM does:
		// Run returns rather than exiting, so everything deferred above still
		// runs. An os.Exit from a keypress would abandon an index pass
		// mid-write.
		tuiCtx, stopDashboard := context.WithCancel(ctx)
		drawn := make(chan struct{})
		var drawErr error
		go func() {
			defer close(drawn)
			drawErr = tui.Run(tuiCtx, bus, tui.Options{
				Source:  *source,
				Addr:    addr,
				LogPath: logPath,
				Watch:   *watchFS,
			})
		}()
		select {
		case <-quit:
		case fatal = <-serveErr:
		case <-drawn:
		}
		// Nothing may print until the alt screen is gone and the terminal is
		// out of raw mode, which is what waiting for Run to return buys — and
		// why a dashboard that died of its own accord reports it only now.
		stopDashboard()
		<-drawn
		logs.toTerminal()
		if drawErr != nil {
			slog.Error("interactive: dashboard stopped", "err", drawErr)
		}
	} else {
		select {
		case <-quit:
		case fatal = <-serveErr:
		}
	}
	if fatal != nil {
		fatalf("server error: %v", fatal)
	}

	slog.Info("shutting down server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), seconds(cfg.Server.ShutdownTimeoutSeconds))
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
	if shutdownPprof != nil {
		if err := shutdownPprof(shutdownCtx); err != nil {
			log.Printf("pprof shutdown error: %v", err)
		}
	}
	if err := server.Close(); err != nil {
		log.Printf("Server close error: %v", err)
	}

	slog.Info("server stopped")
}

// command is the subcommand an invocation asked for, with whatever it parsed
// out of that subcommand's own words.
type command struct {
	name      string
	repos     reposArgs // meaningful only when name is commandRepos
	mcp       []string  // meaningful only when name is commandMCP
	initPath  string    // meaningful only when name is commandInit; "" means the --config path
	skillsDir string    // meaningful only when name is commandSkills; "" means defaultSkillsDir
}

// parseCommand validates the positional arguments flag parsing left behind.
// Every word is accounted for here, before anything opens a database: a
// mistyped subcommand should cost a usage message, not a startup.
func parseCommand(args []string) (command, error) {
	if len(args) == 0 {
		return command{name: commandRun}, nil
	}
	switch args[0] {
	case commandRun:
		if len(args) > 1 {
			return command{}, fmt.Errorf("unexpected arguments after %q: %s", args[0], strings.Join(args[1:], " "))
		}
		return command{name: commandRun}, nil
	case commandRepos:
		sub, err := parseReposArgs(args[1:])
		if err != nil {
			return command{}, err
		}
		return command{name: commandRepos, repos: sub}, nil
	case commandMCP:
		// Everything after the word belongs to the subcommand's own flag set
		// — `ragota mcp -check` — and is parsed and rejected there.
		return command{name: commandMCP, mcp: args[1:]}, nil
	case commandInit:
		path, err := parseInitArgs(args[1:])
		if err != nil {
			return command{}, err
		}
		return command{name: commandInit, initPath: path}, nil
	case commandSkills:
		dir, err := parseSkillsArgs(args[1:])
		if err != nil {
			return command{}, err
		}
		return command{name: commandSkills, skillsDir: dir}, nil
	default:
		return command{}, fmt.Errorf("unknown command %q", args[0])
	}
}

// usage prints the invocation forms and the flags.
func usage() {
	_, _ = fmt.Fprintf(flag.CommandLine.Output(), `usage: ragota [flags] [command]

  %[1]s                    start the HTTP API; the default, and may be omitted
  %[2]s list             every repository in the index, and which are active
  %[2]s activate REPO    put one repository back into the working set
  %[2]s deactivate REPO  take one out of it, keeping its index
  %[3]s [flags]          serve the index to a coding agent: MCP over stdio
                         (configured by the launch block's environment; see
                         ragota %[3]s -h)
  %[4]s [path]           write the annotated example config and exit; the
                         default path is what --config or RAGOTA_CONFIG names
  %[5]s install [dir]   write this binary's agent skills into a skills
                         directory (default %[6]s)

examples:
  ragota --config config.yaml
  ragota --source ./projects %[1]s
  ragota --source ./projects --watch %[1]s
  ragota --source ./projects --watch --interactive %[1]s
  ragota %[2]s list
  ragota %[3]s -check
  ragota %[4]s
  ragota %[5]s install

flags:
`, commandRun, commandRepos, commandMCP, commandInit, commandSkills, defaultSkillsDir)
	flag.PrintDefaults()
}

// activateSource makes the repositories a --source run found into the working
// set: exactly those active, every other registered repository dormant. It is
// the whole of that decision, and there are two ways it declines to make it.
//
// A run with no --source changes nothing. The set is then whatever the user
// last chose — through an earlier --source or through `repos activate` — and a
// plain `ragota --config config.yaml` has been given no reason to redefine
// it. (--check-config never reaches this at all: it exits above.)
//
// A source that matched nothing changes nothing either. found is empty when
// every repository under the directory failed to register, and
// SetActiveRepos would honour the empty list literally, leaving an index that
// answers nothing until the user works out which of two things went wrong.
// A mistyped path is much the likelier of them, and warning while leaving the
// previous set alone is recoverable from in a way an emptied set is not.
func activateSource(ctx context.Context, svc *app.Service, source string, found []*domain.Repo) error {
	if source == "" || len(found) == 0 {
		return nil
	}
	return bootstrap.ActivateOnly(ctx, svc, found)
}

// loadConfig reads the config file, falling back to the built-in local profile
// when the default config path holds nothing and allowProfile says this
// invocation can run without one.
//
// The fallback is narrow on purpose. A --config or RAGOTA_CONFIG that names a
// missing file stays a hard error: the user named a file and it is not there.
// Only the default path — the one nobody asked for — may be found missing, and
// only where "point it at a directory and go" is the whole request and
// hand-writing a config file to satisfy it is what is being removed.
func loadConfig(flagValue string, allowProfile bool) (*config.Config, error) {
	path := resolveConfigPath(flagValue)
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	named := flagValue != "" || os.Getenv("RAGOTA_CONFIG") != ""
	if !allowProfile || named || !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	slog.Info("no config file found; using the built-in local profile", "tried", path)
	return config.LocalDefault(), nil
}

// startWatcher follows the local repositories in the working set and returns
// the function that stops it, or nil when no watcher is running.
//
// The active set and not every registered repository, for a reason beyond
// tidiness: on the kqueue platforms a watch costs a file descriptor per
// directory entry, which is why the watcher is bounded at all (watch/budget.go)
// — and a budget spent following repositories this run is not about is what
// makes the bound bite on the ones it is about.
//
// Local repositories only. A git-sourced repository is refreshed by pulling
// it, and the incremental pass syncs a repository's source before reading
// files that arrive without content — so watching one would turn an editor's
// save into a git pull. The working tree a person edits is the local kind.
//
// Nothing here is fatal. A watcher that cannot start leaves a server that
// indexes on request, which is the server everyone had before this flag.
func startWatcher(ctx context.Context, bg *sync.WaitGroup, svc *app.Service, bus *progress.Bus) func() {
	active, err := svc.ActiveRepos(ctx)
	if err != nil {
		slog.Error("watch: cannot list the active repositories", "err", err)
		return nil
	}
	var local []*domain.Repo
	for _, r := range active {
		if r.Source == domain.SourceTypeLocal {
			local = append(local, r)
		}
	}
	if len(local) == 0 {
		slog.Warn("watch: no local repositories in the working set to watch")
		return nil
	}

	w, err := watch.New(svc, watch.Options{Bus: bus})
	if err != nil {
		slog.Error("watch: cannot start", "err", err)
		return nil
	}
	for _, r := range local {
		if err := w.Add(r); err != nil {
			slog.Warn("watch: cannot follow repository", "repo_id", r.ID, "err", err)
		}
	}
	bg.Add(1)
	go func() {
		defer bg.Done()
		w.Run(ctx)
	}()
	return func() { _ = w.Close() }
}

// resolveConfigPath applies the precedence --config > RAGOTA_CONFIG > default.
func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("RAGOTA_CONFIG"); env != "" {
		return env
	}
	return config.DefaultConfigPath
}

// seconds converts a non-negative config value to a duration; 0 disables the
// timeout, which is what net/http expects.
func seconds(v int) time.Duration {
	if v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Second
}

// newHandler builds the process log handler over w. It hands back a handler
// rather than a logger so that the status bus can wrap it (see
// progress.NewLogHandler) without repeating the format and level decisions, and
// takes the destination because --interactive moves it off the terminal the
// dashboard is drawing on.
func newHandler(cfg *config.LogConfig, w io.Writer) slog.Handler {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	if cfg.Format == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func versionString() string {
	rev := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				rev = " (" + s.Value[:7] + ")"
			}
		}
	}
	return fmt.Sprintf("ragota %s%s %s/%s %s", version, rev, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
