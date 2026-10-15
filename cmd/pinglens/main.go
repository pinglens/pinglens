package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinglens/pinglens/internal/agent"
	"github.com/pinglens/pinglens/internal/coordinator"
	"github.com/pinglens/pinglens/web"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode := pinglensMode()
	switch mode {
	case "coordinator":
		if err := runCoordinator(ctx, logger); err != nil {
			logger.Error("coordinator stopped", "err", err)
			os.Exit(1)
		}
	case "agent":
		if err := runAgent(ctx, logger); err != nil {
			logger.Error("agent stopped", "err", err)
			os.Exit(1)
		}
	default:
		logger.Error("invalid mode", "mode", mode)
		os.Exit(2)
	}
}

func pinglensMode() string {
	if mode := strings.TrimSpace(os.Getenv("PINGLENS_MODE")); mode != "" {
		return mode
	}
	if hasAgentConfig() {
		return "agent"
	}
	return "coordinator"
}

func hasAgentConfig() bool {
	return strings.TrimSpace(os.Getenv("PINGLENS_AGENT_ID")) != "" && strings.TrimSpace(os.Getenv("PINGLENS_COORDINATOR_URL")) != ""
}

func runCoordinator(ctx context.Context, logger *slog.Logger) error {
	ui, err := web.Handler(web.Config{
		AppName:      env("PINGLENS_APP_NAME", "PingLens"),
		BrandName:    os.Getenv("PINGLENS_BRAND_NAME"),
		BrandURL:     os.Getenv("PINGLENS_BRAND_URL"),
		BrandLogoURL: os.Getenv("PINGLENS_BRAND_LOGO_URL"),
		Theme: web.Theme{
			Accent:          strings.TrimSpace(os.Getenv("PINGLENS_THEME_ACCENT")),
			AccentSecondary: strings.TrimSpace(os.Getenv("PINGLENS_THEME_ACCENT_SECONDARY")),
			Background:      strings.TrimSpace(os.Getenv("PINGLENS_THEME_BACKGROUND")),
		},
	})
	if err != nil {
		return err
	}
	cfg := coordinator.Config{
		ListenAddr:             env("PINGLENS_LISTEN", ":8080"),
		MetricsListenAddr:      env("PINGLENS_METRICS_LISTEN", ":9090"),
		Regions:                splitCSV(os.Getenv("PINGLENS_REGIONS")),
		AgentTokens:            parseTokens(os.Getenv("PINGLENS_AGENT_TOKENS")),
		UI:                     ui,
		PingCount:              envIntAllowZero("PINGLENS_PING_COUNT", 0),
		PingInterval:           envDuration("PINGLENS_PING_INTERVAL", time.Second),
		PingTimeout:            envDuration("PINGLENS_PING_TIMEOUT", 3*time.Second),
		JobDuration:            envDuration("PINGLENS_JOB_DURATION", 10*time.Minute),
		MTRCycles:              envInt("PINGLENS_MTR_CYCLES", 10),
		MTRMaxTTL:              envInt("PINGLENS_MTR_MAX_TTL", 30),
		MTRInterval:            envDuration("PINGLENS_MTR_INTERVAL", 5*time.Second),
		AgentPollTimeout:       envDuration("PINGLENS_AGENT_POLL_TIMEOUT", 30*time.Second),
		AgentOnlineTimeout:     envDuration("PINGLENS_AGENT_ONLINE_TIMEOUT", 45*time.Second),
		JobRetention:           envDuration("PINGLENS_JOB_RETENTION", 15*time.Minute),
		GlobalCreateRateLimit:  envInt("PINGLENS_GLOBAL_CREATE_RATE_LIMIT", 120),
		TargetCreateRateLimit:  envInt("PINGLENS_TARGET_CREATE_RATE_LIMIT", 30),
		ClientCreateRateLimit:  envInt("PINGLENS_CLIENT_CREATE_RATE_LIMIT", 20),
		CreateRateWindow:       envDuration("PINGLENS_CREATE_RATE_WINDOW", time.Minute),
		GlobalConcurrencyLimit: envInt("PINGLENS_GLOBAL_CONCURRENCY_LIMIT", 100),
		TargetConcurrencyLimit: envInt("PINGLENS_TARGET_CONCURRENCY_LIMIT", 25),
		ClientConcurrencyLimit: envInt("PINGLENS_CLIENT_CONCURRENCY_LIMIT", 10),
		MaxQueuedJobsPerAgent:  envInt("PINGLENS_MAX_QUEUED_JOBS_PER_AGENT", 16),
		ValkeyURL:              os.Getenv("PINGLENS_VALKEY_URL"),
		ValkeySentinelAddrs:    splitCSV(os.Getenv("PINGLENS_VALKEY_SENTINEL_ADDRS")),
		ValkeySentinelMaster:   env("PINGLENS_VALKEY_SENTINEL_MASTER", "myprimary"),
		ValkeyPassword:         os.Getenv("PINGLENS_VALKEY_PASSWORD"),
		ValkeyPrefix:           env("PINGLENS_VALKEY_PREFIX", "pinglens"),
	}
	srv, err := coordinator.NewServer(cfg, logger)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: srv}
	metricsServer := &http.Server{Addr: cfg.MetricsListenAddr, Handler: srv.MetricsHandler()}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("starting coordinator", "listen", cfg.ListenAddr, "regions", strings.Join(cfg.Regions, ","))
		errCh <- httpServer.ListenAndServe()
	}()
	go func() {
		logger.Info("starting coordinator metrics", "listen", cfg.MetricsListenAddr)
		errCh <- metricsServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func runAgent(ctx context.Context, logger *slog.Logger) error {
	token, err := tokenValue()
	if err != nil {
		return err
	}
	cfg := agent.Config{
		CoordinatorURL: strings.TrimRight(strings.TrimSpace(os.Getenv("PINGLENS_COORDINATOR_URL")), "/"),
		ClusterID:      strings.TrimSpace(os.Getenv("PINGLENS_AGENT_ID")),
		DisplayName:    strings.TrimSpace(os.Getenv("PINGLENS_DISPLAY_NAME")),
		Token:          token,
		MTRBinary:      env("PINGLENS_MTR_BINARY", "mtr"),
		UserAgent:      env("PINGLENS_USER_AGENT", "pinglens-agent"),
		PollBackoff:    envDuration("PINGLENS_POLL_BACKOFF", 2*time.Second),
	}
	if cfg.CoordinatorURL == "" {
		return fmt.Errorf("PINGLENS_COORDINATOR_URL is required")
	}
	if cfg.ClusterID == "" {
		return fmt.Errorf("PINGLENS_AGENT_ID is required")
	}
	return agent.Run(ctx, cfg, logger)
}

func tokenValue() (string, error) {
	if token := os.Getenv("PINGLENS_AGENT_TOKEN"); token != "" {
		return token, nil
	}
	path := os.Getenv("PINGLENS_AGENT_TOKEN_FILE")
	if path == "" {
		return "", fmt.Errorf("PINGLENS_AGENT_TOKEN or PINGLENS_AGENT_TOKEN_FILE is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("agent token file is empty")
	}
	return token, nil
}

func parseTokens(raw string) map[string]string {
	out := map[string]string{}
	for _, item := range splitCSV(raw) {
		cluster, token, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		cluster = strings.TrimSpace(cluster)
		token = strings.TrimSpace(token)
		if cluster != "" && token != "" {
			out[cluster] = token
		}
	}
	return out
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envIntAllowZero(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("PINGLENS_LOG_LEVEL")) {
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
