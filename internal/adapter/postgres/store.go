package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// PoolConfig limits the connection pool. Without a limit, a burst of requests waiting on the
// merchant row lock keeps opening connections until Postgres refuses new ones.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open connects to the database at url and checks that it is reachable.
func Open(ctx context.Context, url string, pool PoolConfig) (*sqlx.DB, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// Store implements the transfer ports on top of a connection pool.
type Store struct {
	db *sqlx.DB
}

var (
	_ transfer.UnitOfWork     = (*Store)(nil)
	_ transfer.TransferReader = (*Store)(nil)
)

// NewStore returns a Store that uses db.
func NewStore(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// Do implements transfer.UnitOfWork.
func (s *Store) Do(ctx context.Context, fn func(r transfer.TxRepos) error) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(txRepos{tx: tx}); err != nil {
		// A canceled context has already rolled the transaction back.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type txRepos struct {
	tx *sqlx.Tx
}

func (r txRepos) Transfers() transfer.TransferWriter { return transferWriter(r) }
func (r txRepos) Outbox() transfer.OutboxWriter      { return outboxWriter(r) }
func (r txRepos) Merchants() transfer.BalanceDebiter { return merchantRepo(r) }
func (r txRepos) Ledger() transfer.LedgerWriter      { return ledgerWriter(r) }
