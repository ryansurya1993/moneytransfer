//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/postgres"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const templateDB = "moneytransfer_template"

var (
	serverURL *url.URL // the container's "postgres" database
	admin     *sqlx.DB // creates and drops the per-test databases
	createMu  sync.Mutex
	dbCounter atomic.Int64

	testPool = postgres.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 10, ConnMaxLifetime: time.Minute}
)

// TestMain starts one Postgres container for the package and migrates a template database.
// Each test then gets its own copy of the template.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	// On Windows, testcontainers looks for Docker by stat-ing its named pipe, which fails with
	// "All pipe instances are busy" while another program is connecting to Docker at the same moment.
	// With DOCKER_HOST set (to the pipe it would find anyway) it dials instead, and a dial waits.
	if runtime.GOOS == "windows" && os.Getenv("DOCKER_HOST") == "" {
		if err := os.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine"); err != nil {
			log.Printf("set DOCKER_HOST: %v", err)
			return 1
		}
	}

	container, err := tcpostgres.Run(ctx, "postgres:18.7-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Printf("start postgres container: %v", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("connection string: %v", err)
		return 1
	}
	if serverURL, err = url.Parse(dsn); err != nil {
		log.Printf("parse connection string: %v", err)
		return 1
	}
	if admin, err = postgres.Open(ctx, dbURL("postgres"), testPool); err != nil {
		log.Printf("open admin connection: %v", err)
		return 1
	}
	defer func() { _ = admin.Close() }()

	if err := createTemplate(ctx); err != nil {
		log.Printf("create template database: %v", err)
		return 1
	}
	return m.Run()
}

func dbURL(name string) string {
	u := *serverURL
	u.Path = "/" + name
	return u.String()
}

func createTemplate(ctx context.Context) error {
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return err
	}
	tmpl, err := postgres.Open(ctx, dbURL(templateDB), testPool)
	if err != nil {
		return err
	}
	err = migrate(ctx, tmpl)
	// Postgres refuses to copy a template that still has open connections.
	return errors.Join(err, tmpl.Close())
}

func migrate(ctx context.Context, db *sqlx.DB) error {
	dir := os.DirFS("../../../migrations")
	files, err := fs.Glob(dir, "*.up.sql")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no migrations found")
	}
	sort.Strings(files)
	for _, f := range files {
		sqlText, err := fs.ReadFile(dir, f)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

// newDB creates a fresh database from the template with merchant 1 holding balance.
func newDB(t *testing.T, balance transfer.Amount) *sqlx.DB {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("test_%d", dbCounter.Add(1))

	// Postgres rejects concurrent copies of the same template.
	createMu.Lock()
	_, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE "+templateDB)
	createMu.Unlock()
	if err != nil {
		t.Fatalf("create database: %v", err)
	}

	db, err := postgres.Open(ctx, dbURL(name), testPool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	const seed = `
		WITH m AS (
			INSERT INTO merchants (id, name, balance, version)
			VALUES (1, 'Test Merchant', $1, 1)
			RETURNING id, balance, version
		)
		INSERT INTO balance_entries (merchant_id, type, amount, balance_after, merchant_version)
		SELECT id, 'OPENING', balance, balance, version
		FROM m
	`
	_, err = db.ExecContext(ctx, seed, balance)
	if err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	return db
}
