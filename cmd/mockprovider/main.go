// Command mockprovider runs a fake payout provider for local runs and the load test.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ryansurya1993/moneytransfer/internal/config"
	"github.com/ryansurya1993/moneytransfer/internal/mockprovider"
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("mockprovider stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadMockProvider()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	logger := slog.New(logHandler)
	logger.Info("starting mockprovider", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr: ":" + strconv.Itoa(cfg.HTTP.Port),
		Handler: mockprovider.NewHandler(mockprovider.Options{
			MinDelay:           cfg.MinDelay,
			MaxDelay:           cfg.MaxDelay,
			FailurePercent:     cfg.FailurePercent,
			TimeoutPercent:     cfg.TimeoutPercent,
			UnavailablePercent: cfg.UnavailablePercent,
			HangDuration:       cfg.HangDuration,
		}, logger),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logHandler, slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", srv.Addr)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
