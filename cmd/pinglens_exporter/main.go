// Command pinglens_exporter traces its configured targets continuously, one
// ICMP probe per hop per interval, and serves per-hop counters and RTT
// histograms for Prometheus.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pinglens/pinglens/internal/exporter"
	"github.com/pinglens/pinglens/internal/traceroute"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger); err != nil {
		logger.Error("exporter stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	configPath := env("PINGLENS_EXPORTER_CONFIG", "/config/config.yml")
	listen := env("PINGLENS_EXPORTER_LISTEN", ":9430")
	reloadInterval := envDuration("PINGLENS_EXPORTER_RELOAD_INTERVAL", 30*time.Second)

	prober, err := traceroute.NewProber(logger)
	if err != nil {
		return err
	}
	defer prober.Close()
	names := traceroute.NewHopNames(net.DefaultResolver, time.Hour)
	exp, err := exporter.New(logger, configPath, prober, names)
	if err != nil {
		return err
	}

	server := &http.Server{Addr: listen, Handler: exp.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("pinglens exporter listening", "addr", listen, "config", configPath)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		exp.Run(runCtx, reloadInterval)
		close(runDone)
	}()

	select {
	case <-runDone:
	case err = <-serveErr:
		cancel()
		<-runDone
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	return err
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
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
