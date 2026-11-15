// Command loadtest sends a burst of transfers to the api and checks the money afterwards.
// It exits 0 when every check passes, 1 when one fails, and 2 when the run could not be judged.
//
// `loadtest topup <amount>` adds money to the merchant's balance instead, with a DEPOSIT ledger entry,
// and exits 0 when it was added and 1 when not.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/config"
	"github.com/ryansurya1993/moneytransfer/internal/loadtest"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func main() {
	os.Exit(run())
}

func run() int {
	isTopup := len(os.Args) > 1 && os.Args[1] == "topup"
	notRun := 2
	if isTopup {
		notRun = 1
	}

	cfg, err := config.LoadLoadTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return notRun
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
	db, err := postgres.Open(openCtx, cfg.DB.URL, postgres.PoolConfig{
		MaxOpenConns:             2,
		MaxIdleConns:             2,
		ConnMaxLifetime:          cfg.DB.ConnMaxLifetime,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	})
	cancelOpen()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return notRun
	}
	defer func() { _ = db.Close() }()

	if isTopup {
		return topup(ctx, postgres.NewStore(db), transfer.MerchantID(cfg.MerchantID), os.Args[2:])
	}

	report, err := loadtest.Run(ctx, db, loadtest.Options{
		TargetURL:        cfg.TargetURL,
		ProviderURL:      cfg.ProviderURL,
		Rate:             cfg.Rate,
		Duration:         cfg.Duration,
		Concurrency:      cfg.Concurrency,
		DuplicatePercent: cfg.DuplicatePercent,
		MagicPercent:     cfg.MagicPercent,
		RequestTimeout:   cfg.RequestTimeout,
		DrainTimeout:     cfg.DrainTimeout,
		MinAmount:        cfg.MinAmount,
		MaxAmount:        cfg.MaxAmount,
		MerchantID:       cfg.MerchantID,
		Progress:         os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load test: %v\n", err)
		return 2
	}
	report.Write(os.Stdout)
	if !report.Passed() {
		return 1
	}
	return 0
}

func topup(ctx context.Context, uow transfer.UnitOfWork, merchantID transfer.MerchantID, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loadtest topup <amount in whole rupiah>")
		return 1
	}
	amount, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "topup: amount %q is not a whole number\n", args[0])
		return 1
	}
	balance, err := transfer.Deposit(ctx, uow, merchantID, transfer.Amount(amount))
	if err != nil {
		fmt.Fprintf(os.Stderr, "topup: %v\n", err)
		return 1
	}
	fmt.Printf("deposited %d to merchant %d, balance now %d\n", amount, merchantID, balance)
	return 0
}
