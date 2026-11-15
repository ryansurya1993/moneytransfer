// Command relay publishes committed outbox events to RabbitMQ, re-queues transfers left without an outcome
// and checks the ledger.
//
// `relay resolve-ledger-finding <id> --by <name> --note <text>` closes a ledger finding instead.
package main

import (
	"context"
	"errors"
	"flag"
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
	"github.com/ryansurya1993/moneytransfer/internal/adapter/rabbitmq"
	"github.com/ryansurya1993/moneytransfer/internal/config"
	"github.com/ryansurya1993/moneytransfer/internal/health"
	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/outbox"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// A ledger check run that hasn't finished for this long is stuck; runs are cut off after 2 minutes.
const ledgerCheckStallTimeout = 5 * time.Minute

func main() {
	if len(os.Args) > 1 && os.Args[1] == "resolve-ledger-finding" {
		os.Exit(resolveFinding(os.Args[2:]))
	}
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("relay stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadRelay()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logHandler := logging.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger := slog.New(logHandler)
	logger.Info("starting relay", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
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

	conn, err := rabbitmq.Dial(cfg.RabbitMQURL, "relay")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.DeclareTopology(cfg.DeliveryLimit); err != nil {
		return err
	}
	pub, err := conn.NewPublisher()
	if err != nil {
		return err
	}

	store := postgres.NewStore(db)
	relay := outbox.NewRelay(store, pub, logger, outbox.Options{
		BatchSize:      cfg.BatchSize,
		PollInterval:   cfg.PollInterval,
		PublishTimeout: cfg.PublishTimeout,
		AlertAge:       cfg.OutboxAlertAge,
	})
	reconciler := transfer.NewReconciler(store, store, logger, transfer.ReconcilerOptions{
		After:       cfg.ReconcileAfter,
		MaxAttempts: cfg.ReconcileMaxAttempts,
	})
	ledgerChecker := transfer.NewLedgerChecker(store, logger)
	srv := &http.Server{
		Addr: ":" + strconv.Itoa(cfg.HTTP.Port),
		Handler: health.NewHandler(
			func() error {
				if age := time.Since(relay.LastRound()); age > cfg.StallTimeout {
					return fmt.Errorf("no relay round finished for %s", age.Round(time.Second))
				}
				if age := time.Since(ledgerChecker.LastRound()); age > ledgerCheckStallTimeout {
					return fmt.Errorf("no ledger check run finished for %s", age.Round(time.Second))
				}
				return nil
			},
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
	g.Go(func() error { return relay.Run(ctx) })
	g.Go(func() error { return reconciler.Run(ctx) })
	g.Go(func() error { return ledgerChecker.Run(ctx) })
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
	// Unpublished rows stay in the outbox, so nothing is lost.
	g.Go(func() error {
		select {
		case err := <-conn.Closed():
			return fmt.Errorf("rabbitmq connection lost: %w", err)
		case <-ctx.Done():
			return nil
		}
	})
	logger.Info("relay running", "health_addr", srv.Addr)
	return g.Wait()
}

func resolveFinding(args []string) int {
	const usage = `usage: relay resolve-ledger-finding <id> --by <name> --note <why it is resolved>`
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 1
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "finding id %q is not a number\n%s\n", args[0], usage)
		return 1
	}
	fs := flag.NewFlagSet("resolve-ledger-finding", flag.ContinueOnError)
	by := fs.String("by", "", "who resolves the finding")
	note := fs.String("note", "", "what was found and done")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	cfg, err := config.LoadLedgerAdmin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	db, err := postgres.Open(ctx, cfg.DB.URL, postgres.PoolConfig{
		MaxOpenConns:             1,
		MaxIdleConns:             1,
		ConnMaxLifetime:          cfg.DB.ConnMaxLifetime,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	merchantID, held, err := transfer.ResolveLedgerFinding(ctx, postgres.NewStore(db), id, *by, *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if held {
		fmt.Printf("finding %d resolved; payouts for merchant %d stay on hold until its other findings are resolved\n", id, merchantID)
	} else {
		fmt.Printf("finding %d resolved; payouts for merchant %d are not on hold\n", id, merchantID)
	}
	return 0
}
