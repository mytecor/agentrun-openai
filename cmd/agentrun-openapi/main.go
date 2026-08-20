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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dmora/agentrun"
	"github.com/dmora/agentrun/engine/acp"
	"github.com/dmora/agentrun/engine/cli"
	"github.com/dmora/agentrun/engine/cli/claude"

	"github.com/mytecor/agentrun-openapi/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var (
		host          = flag.String("host", env("AGENTRUN_HOST", "127.0.0.1"), "HTTP listen host")
		port          = flag.Int("port", envInt("AGENTRUN_PORT", 8787), "HTTP listen port")
		apiKey        = flag.String("api-key", os.Getenv("AGENTRUN_API_KEY"), "optional bearer token")
		defaultCWD    = flag.String("default-cwd", os.Getenv("AGENTRUN_DEFAULT_CWD"), "default agent working directory")
		claudeBinary  = flag.String("claude-binary", env("AGENTRUN_CLAUDE_BINARY", "claude"), "Claude Code binary")
		codexBinary   = flag.String("codex-acp-binary", env("AGENTRUN_CODEX_ACP_BINARY", "codex-acp"), "Codex ACP binary")
		codexArgs     = flag.String("codex-acp-args", os.Getenv("AGENTRUN_CODEX_ACP_ARGS"), "comma-separated Codex ACP arguments")
		turnTimeout   = flag.Duration("turn-timeout", envDuration("AGENTRUN_TURN_TIMEOUT", 30*time.Minute), "maximum duration of one agent turn")
		sessionTTL    = flag.Duration("session-ttl", envDuration("AGENTRUN_SESSION_TTL", time.Hour), "idle session lifetime")
		shutdownGrace = flag.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	)
	flag.Parse()
	if strings.TrimSpace(*host) == "" {
		return errors.New("host must not be empty")
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", *port)
	}
	listenAddr := net.JoinHostPort(*host, strconv.Itoa(*port))

	if *defaultCWD == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		*defaultCWD = cwd
	}
	engines := map[string]agentrun.Engine{
		"claude-code": cli.NewEngine(claude.New(claude.WithBinary(*claudeBinary))),
		"codex": acp.NewEngine(
			acp.WithBinary(*codexBinary),
			acp.WithArgs(splitArgs(*codexArgs)...),
			acp.WithStderrWriter(os.Stderr),
		),
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	handler := gateway.New(gateway.Config{
		Engines:     engines,
		DefaultCWD:  *defaultCWD,
		APIKey:      *apiKey,
		TurnTimeout: *turnTimeout,
		SessionTTL:  *sessionTTL,
		Logger:      logger,
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

func splitArgs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}
