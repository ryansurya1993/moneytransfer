package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// IdleInTransactionTimeout makes Postgres end a session idling inside an open transaction.
	// A transaction left open by a failed rollback then can't hold its row locks for long.
	// Zero keeps the server default, which is no limit.
	IdleInTransactionTimeout time.Duration
}

// Open connects to the database at url and checks that it is reachable.
func Open(ctx context.Context, url string, pool PoolConfig) (*sqlx.DB, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if pool.IdleInTransactionTimeout > 0 {
		cfg.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(pool.IdleInTransactionTimeout.Milliseconds(), 10)
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
		return fmt.Errorf("begin transaction: %w", wrapUnavailable(err))
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(txRepos{tx: tx}); err != nil {
		err = wrapUnavailable(err)
		// A canceled context has already rolled the transaction back.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", wrapUnavailable(err))
	}
	return nil
}

type txRepos struct {
	tx *sqlx.Tx
}

func (r txRepos) Transfers() transfer.TransferWriter { return transferWriter(r) }
func (r txRepos) Outbox() transfer.OutboxWriter      { return outboxWriter(r) }
func (r txRepos) Merchants() transfer.BalanceUpdater { return merchantRepo(r) }
func (r txRepos) Ledger() transfer.LedgerWriter      { return ledgerWriter(r) }

// wrapUnavailable marks a lost or refused connection with transfer.ErrUnavailable.
// Callers can then tell an outage from a bug.
func wrapUnavailable(err error) error {
	if err != nil && !errors.Is(err, transfer.ErrUnavailable) && isConnectionLost(err) {
		return fmt.Errorf("%w: %w", transfer.ErrUnavailable, err)
	}
	return err
}

// isConnectionLost reports whether err comes from not reaching the server or losing the connection.
// A restart, for one, ends pooled connections with SQLSTATE 57P01, not a connect error.
func isConnectionLost(err error) bool {
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 is a connection exception; 57P is the server shutting down or not accepting connections.
		return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57P")
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, driver.ErrBadConn)
}
