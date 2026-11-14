// Command api serves the transfer HTTP API.
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
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/httpapi"
	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/config"
	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("api stopped", "error", err)
		os.Exit(1)
	}
}

// healthcheck asks the api running in this container whether it is alive.
func healthcheck() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:"+strconv.Itoa(cfg.HTTP.Port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", resp.Status)
	}
	return nil
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logHandler := logging.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger := slog.New(logHandler)
	logger.Info("starting api", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, cfg.DBTimeout)
	db, err := postgres.Open(openCtx, cfg.DB.URL, postgres.PoolConfig{
		MaxOpenConns:             cfg.DB.MaxOpenConns,
		MaxIdleConns:             cfg.DB.MaxIdleConns,
		ConnMaxLifetime:          cfg.DB.ConnMaxLifetime,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	})
	cancelOpen()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	store := postgres.NewStore(db)
	svc := transfer.NewService(store, store, transfer.Amount(cfg.MaxTransferAmount))
	srv := &http.Server{
		Addr: ":" + strconv.Itoa(cfg.HTTP.Port),
		Handler: httpapi.NewHandler(svc, db, logger, httpapi.Options{
			MerchantID: transfer.MerchantID(cfg.MerchantID),
			DBTimeout:  cfg.DBTimeout,
		}),
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

	// Let in-flight requests finish, so their clients get a response instead of a reset connection.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}
