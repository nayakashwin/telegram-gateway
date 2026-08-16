package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nayakashwin/telegram-gateway/internal/api"
	"github.com/nayakashwin/telegram-gateway/internal/config"
	"github.com/nayakashwin/telegram-gateway/internal/gateway"
	"github.com/nayakashwin/telegram-gateway/internal/metrics"
	"github.com/nayakashwin/telegram-gateway/internal/store"
	"github.com/nayakashwin/telegram-gateway/internal/telegram"
)

func main() {
	// Bootstrap a JSON logger before config is loaded.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(".env")
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	logger.Info("config loaded", "level", cfg.LogLevel, "api_address", cfg.APIAddress, "metrics_address", cfg.MetricsAddress)

	// Rebuild the logger at the configured level.
	logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	st, err := store.New(ctx, cfg.DatabaseURL, store.PoolConfig{
		MinConns:        cfg.DBPool.MinConns,
		MaxConns:        cfg.DBPool.MaxConns,
		MaxConnLifetime: cfg.DBPool.MaxConnLifetime,
		MaxConnIdleTime: cfg.DBPool.MaxConnIdleTime,
	})
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	m := metrics.New()
	m.RegisterPool(st)

	defaultClient := telegram.New(cfg.DefaultBot().Token, logger)
	defaultClient.SetMetrics(m)

	gateway := gateway.New(cfg, st, defaultClient, logger, m)
	for _, b := range cfg.Bots[1:] {
		c := telegram.New(b.Token, logger)
		c.SetMetrics(m)
		gateway.RegisterBot(b.Name, c)
	}
	apiServer := api.New(cfg, st, logger, m)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return apiServer.ListenAndServe(gctx) })
	if cfg.MetricsAddress != "" {
		g.Go(func() error {
			logger.Info("metrics server listening", "address", cfg.MetricsAddress)
			return api.ListenAndServe(gctx, cfg.MetricsAddress, m.Handler(), 5*time.Second)
		})
	}
	g.Go(func() error { return gateway.Run(gctx) })

	if err := g.Wait(); err != nil {
		logger.Error("component failed; shutting down", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
