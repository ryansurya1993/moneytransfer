package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

func TestUnreachableDatabaseIsUnavailable(t *testing.T) {
	// Nothing listens on port 1, so every connection attempt is refused right away.
	cfg, err := pgx.ParseConfig("postgres://user:secret@127.0.0.1:1/db?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db)
	ctx := context.Background()

	_, err = s.ByID(ctx, uuid.New())
	if !errors.Is(err, transfer.ErrUnavailable) {
		t.Errorf("ByID: got %v, want ErrUnavailable", err)
	}

	err = s.Do(ctx, func(transfer.TxRepos) error { return nil })
	if !errors.Is(err, transfer.ErrUnavailable) {
		t.Errorf("Do: got %v, want ErrUnavailable", err)
	}
}

func TestLostConnectionErrorsAreUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "server shutting down", err: &pgconn.PgError{Code: "57P01"}, want: true},
		{name: "server crashed", err: &pgconn.PgError{Code: "57P02"}, want: true},
		{name: "server starting up", err: &pgconn.PgError{Code: "57P03"}, want: true},
		{name: "connection failure", err: &pgconn.PgError{Code: "08006"}, want: true},
		{name: "connection cut mid-query", err: fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), want: true},
		{name: "unique violation", err: &pgconn.PgError{Code: "23505"}, want: false},
		{name: "statement canceled", err: &pgconn.PgError{Code: "57014"}, want: false},
		{name: "some other error", err: errors.New("syntax error"), want: false},
		{name: "domain error", err: transfer.ErrInsufficientBalance, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errors.Is(wrapUnavailable(tt.err), transfer.ErrUnavailable)
			if got != tt.want {
				t.Errorf("unavailable = %v, want %v", got, tt.want)
			}
			if !errors.Is(wrapUnavailable(tt.err), tt.err) {
				t.Error("the original error is no longer in the chain")
			}
		})
	}
}
