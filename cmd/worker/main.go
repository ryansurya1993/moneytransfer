// Command worker pays out transfers from the RabbitMQ queue through the payout provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/adapter/provider"
	"github.com/ryansurya1993/moneytransfer/internal/adapter/rabbitmq"
	"github.com/ryansurya1993/moneytransfer/internal/config"
	"github.com/ryansurya1993/moneytransfer/internal/health"
	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logHandler := logging.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger := slog.New(logHandler)
	logger.Info("starting worker", "config", cfg)

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

	conn, err := rabbitmq.Dial(cfg.RabbitMQURL, "worker")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.DeclareTopology(cfg.DeliveryLimit); err != nil {
		return err
	}

	processor := transfer.NewProcessor(
		postgres.NewStore(db),
		provider.NewClient(cfg.ProviderURL, cfg.ProviderTimeout, logger),
		logger,
		transfer.ProcessorOptions{
			DBTimeout:        cfg.DBTimeout,
			MaxAttempts:      cfg.ProviderMaxAttempts,
			BackoffBase:      cfg.ProviderBackoffBase,
			BackoffMax:       cfg.ProviderBackoffMax,
			StoreBackoffBase: time.Second,
			StoreBackoffMax:  30 * time.Second,
		},
	)
	srv := &http.Server{
		Addr: ":" + strconv.Itoa(cfg.HTTP.Port),
		// Every step of a message has a timeout, so an answering process is a live one.
		Handler: health.NewHandler(
			func() error { return nil },
			func(ctx context.Context) error {
				if !conn.IsOpen() {
					return errors.New("rabbitmq connection closed")
				}
				return db.PingContext(ctx)
			},
		),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logHandler, slog.LevelWarn),
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return conn.ConsumeTransfers(ctx, processor, rabbitmq.ConsumeOptions{
			Concurrency:     cfg.Concurrency,
			ShutdownTimeout: cfg.ShutdownTimeout,
		}, logger)
	})
	g.Go(func() error {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve health checks: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	// Crash-only: when the broker connection goes, exit and let the platform restart us.
	// Unacked messages go back to the queue, and a transfer in progress is claimed again.
	g.Go(func() error {
		select {
		case err := <-conn.Closed():
			return fmt.Errorf("rabbitmq connection lost: %w", err)
		case <-ctx.Done():
			return nil
		}
	})
	logger.Info("worker running", "concurrency", cfg.Concurrency, "health_addr", srv.Addr)
	return g.Wait()
}
