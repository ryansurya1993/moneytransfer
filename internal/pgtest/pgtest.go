//go:build integration

package pgtest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// Server is a Postgres container with a migrated template database.
type Server struct {
	container *tcpostgres.PostgresContainer
	url       *url.URL
	admin     *sqlx.DB
	// Postgres rejects concurrent copies of the same template.
	createMu sync.Mutex
	count    atomic.Int64
}

// PoolConfig returns the pool settings the test databases are opened with.
func PoolConfig() postgres.PoolConfig {
	return postgres.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 10, ConnMaxLifetime: time.Minute}
}

// Start starts a Postgres container and runs the *.up.sql files in migrations, in name order,
// on a template database. Close stops the container.
func Start(ctx context.Context, migrations fs.FS) (_ *Server, err error) {
	// On Windows, testcontainers looks for Docker by stat-ing its named pipe, which fails with
	// "All pipe instances are busy" while another program is connecting to Docker at the same moment.
	// With DOCKER_HOST set (to the pipe it would find anyway) it dials instead, and a dial waits.
	if runtime.GOOS == "windows" && os.Getenv("DOCKER_HOST") == "" {
		if err := os.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine"); err != nil {
			return nil, fmt.Errorf("set DOCKER_HOST: %w", err)
		}
	}

	container, err := tcpostgres.Run(ctx, "postgres:18.7-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	s := &Server{container: container}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}
	if s.url, err = url.Parse(dsn); err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	if s.admin, err = postgres.Open(ctx, s.URL("postgres"), PoolConfig()); err != nil {
		return nil, fmt.Errorf("open admin connection: %w", err)
	}
	if err := s.createTemplate(ctx, migrations); err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	return s, nil
}

// Close closes the admin connection and stops the container.
func (s *Server) Close() error {
	var err error
	if s.admin != nil {
		err = s.admin.Close()
	}
	return errors.Join(err, s.container.Terminate(context.Background()))
}

// URL returns the connection URL of the database called name.
func (s *Server) URL(name string) string {
	u := *s.url
	u.Path = "/" + name
	return u.String()
}

// NewDB returns a fresh copy of the template where merchant 1 holds balance, with its OPENING entry.
// The database is dropped when the test ends.
func (s *Server) NewDB(t testing.TB, balance transfer.Amount) *sqlx.DB {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("test_%d", s.count.Add(1))

	s.createMu.Lock()
	_, err := s.admin.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE "+templateDB)
	s.createMu.Unlock()
	if err != nil {
		t.Fatalf("create database: %v", err)
	}

	db, err := postgres.Open(ctx, s.URL(name), PoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = s.admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
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
	if _, err := db.ExecContext(ctx, seed, balance); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	return db
}

func (s *Server) createTemplate(ctx context.Context, migrations fs.FS) error {
	if _, err := s.admin.ExecContext(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return err
	}
	tmpl, err := postgres.Open(ctx, s.URL(templateDB), PoolConfig())
	if err != nil {
		return err
	}
	err = migrate(ctx, tmpl, migrations)
	// Postgres refuses to copy a template that still has open connections.
	return errors.Join(err, tmpl.Close())
}

// migrate runs each file as one statement batch, as golang-migrate does.
func migrate(ctx context.Context, db *sqlx.DB, migrations fs.FS) error {
	files, err := fs.Glob(migrations, "*.up.sql")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no migrations found")
	}
	sort.Strings(files)
	for _, f := range files {
		sqlText, err := fs.ReadFile(migrations, f)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}
