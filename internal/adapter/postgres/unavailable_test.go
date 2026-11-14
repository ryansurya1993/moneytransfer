package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
