//go:build integration

package postgres_test

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/ryansurya1993/moneytransfer/internal/pgtest"
)

// pg is the package's Postgres container; each test takes its own database from it.
var pg *pgtest.Server

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var err error
	if pg, err = pgtest.Start(context.Background(), os.DirFS("../../../migrations")); err != nil {
		log.Print(err)
		return 1
	}
	defer func() { _ = pg.Close() }()
	return m.Run()
}
