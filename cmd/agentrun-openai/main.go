package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dmora/agentrun"

	"github.com/mytecor/agentrun-openai/internal/gateway"
)

// version identifies the build. Release binaries set it with
// -ldflags "-X main.version=<tag>"; source builds report "dev".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	allowedRoots := pathListFlag(splitPathList(os.Getenv("AGENTRUN_ALLOWED_ROOTS")))
	var acpFlags acpFlagList
	var effortFlags effortFormatFlagList
	var (
		host          = flag.String("host", env("AGENTRUN_HOST", "127.0.0.1"), "HTTP listen host")
		port          = flag.Int("port", envInt("AGENTRUN_PORT", 8787), "HTTP listen port")
		apiKey        = flag.String("api-key", os.Getenv("AGENTRUN_API_KEY"), "optional bearer token")
		defaultCWD    = flag.String("default-cwd", os.Getenv("AGENTRUN_DEFAULT_CWD"), "default agent working directory")
		turnTimeout   = flag.Duration("turn-timeout", envDuration("AGENTRUN_TURN_TIMEOUT", 30*time.Minute), "maximum duration of one agent turn")
		sessionTTL    = flag.Duration("session-ttl", envDuration("AGENTRUN_SESSION_TTL", 10*time.Minute), "idle process lifetime")
		sessionStore  = flag.String("session-store", env("AGENTRUN_SESSION_STORE", defaultSessionStore()), "native session metadata file (empty disables persistence)")
		heartbeat     = flag.Duration("stream-heartbeat", envDuration("AGENTRUN_STREAM_HEARTBEAT", 20*time.Second), "idle interval before a keep-alive stream delta is sent (negative disables)")
		shutdownGrace = flag.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
		showVersion   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Var(&allowedRoots, "allowed-root", "allowed agent working-directory root (repeatable; empty allows any absolute path)")
	flag.Var(&acpFlags, "acp", "generic ACP backend specification: id=command [args...] (repeatable)")
	flag.Var(&effortFlags, "effort-format", "reasoning effort model parsing format: [id=]format (repeatable; e.g. codex=bracket, bracket, none)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if strings.TrimSpace(*host) == "" {
		return errors.New("host must not be empty")
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", *port)
	}
	listenAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	resolvedRoots, err := resolveRoots(allowedRoots)
	if err != nil {
		return err
	}

	specs := []string(acpFlags)
	if len(specs) == 0 {
		specs = splitACPList(os.Getenv("AGENTRUN_ACP"))
	}
	if len(specs) == 0 {
		return errors.New("no ACP backends configured: specify at least one via --acp or AGENTRUN_ACP (e.g. --acp codex=\"npx @agentclientprotocol/codex-acp\")")
	}
	acpBackends, err := gateway.ParseACPBackends(specs)
	if err != nil {
		return fmt.Errorf("configure acp backends: %w", err)
	}

	effortSpecs := []string(effortFlags)
	if len(effortSpecs) == 0 {
		effortSpecs = gateway.SplitEffortFormatList(os.Getenv("AGENTRUN_EFFORT_FORMAT"))
	}
	effortConfig, err := gateway.ParseEffortFormatSpecs(effortSpecs)
	if err != nil {
		return fmt.Errorf("configure effort formats: %w", err)
	}

	if *defaultCWD == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		*defaultCWD = cwd
	}
	engines := make(map[string]agentrun.Engine, len(acpBackends))
	modelDetails := make(map[string]gateway.ModelDetails, len(acpBackends))
	for _, b := range acpBackends {
		engines[b.ID] = b.NewEngine(os.Stderr)
		modelDetails[b.ID] = gateway.ModelDetails{Name: b.ID}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	handler := gateway.New(gateway.Config{
		Engines:             engines,
		EffortFormats:       effortConfig.PerBackend,
		DefaultEffortFormat: effortConfig.Default,
		ModelDetails:        modelDetails,
		DefaultCWD:          *defaultCWD,
		AllowedRoots:        resolvedRoots,
		APIKey:              *apiKey,
		TurnTimeout:         *turnTimeout,
		SessionTTL:          *sessionTTL,
		SessionStore:        *sessionStore,
		StreamHeartbeat:     *heartbeat,
		Logger:              logger,
	})
	defer handler.Close()

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("agentrun OpenAPI server listening", "addr", listenAddr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownGrace)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

type pathListFlag []string

func (f *pathListFlag) String() string { return strings.Join(*f, string(os.PathListSeparator)) }

func (f *pathListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("allowed root must not be empty")
	}
	*f = append(*f, value)
	return nil
}

type acpFlagList []string

func (f *acpFlagList) String() string { return strings.Join(*f, "; ") }

func (f *acpFlagList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("acp backend specification must not be empty")
	}
	*f = append(*f, value)
	return nil
}

type effortFormatFlagList []string

func (f *effortFormatFlagList) String() string { return strings.Join(*f, "; ") }

func (f *effortFormatFlagList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("effort format specification must not be empty")
	}
	*f = append(*f, value)
	return nil
}

func splitACPList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result []string
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '\n' || r == ';'
	}) {
		if entry = strings.TrimSpace(entry); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

func splitPathList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return filepath.SplitList(value)
}

func resolveRoots(roots []string) ([]string, error) {
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("allowed root must be absolute: %s", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed root %s: %w", root, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat allowed root %s: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("allowed root is not a directory: %s", root)
		}
		result = append(result, resolved)
	}
	return result, nil
}

func defaultSessionStore() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "agentrun-openai", "sessions.json")
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}
