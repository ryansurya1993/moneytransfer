package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/config"
)

const dbURL = "postgres://app:s3cret@db:5432/app?sslmode=disable" //nolint:gosec // fake test credentials

func TestLoadAPIDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", dbURL)

	c, err := config.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if c.DB.URL != dbURL {
		t.Errorf("DB.URL = %q, want %q", c.DB.URL, dbURL)
	}
	if c.DB.MaxOpenConns != 20 {
		t.Errorf("DB.MaxOpenConns = %d, want 20", c.DB.MaxOpenConns)
	}
	if c.DBTimeout != 3*time.Second {
		t.Errorf("DBTimeout = %v, want 3s", c.DBTimeout)
	}
	if c.HTTP.Port != 8080 {
		t.Errorf("HTTP.Port = %d, want 8080", c.HTTP.Port)
	}
	if c.MaxTransferAmount != 250_000_000 {
		t.Errorf("MaxTransferAmount = %d, want 250000000", c.MaxTransferAmount)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", c.LogLevel)
	}
}

func TestLoadAPIEmptyValueUsesDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", dbURL)
	t.Setenv("DB_TIMEOUT", "")

	c, err := config.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBTimeout != 3*time.Second {
		t.Errorf("DBTimeout = %v, want 3s", c.DBTimeout)
	}
}

func TestLoadAPIErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "database url missing",
			env:     map[string]string{},
			wantErr: "DATABASE_URL",
		},
		{
			name:    "database url empty",
			env:     map[string]string{"DATABASE_URL": ""},
			wantErr: "DATABASE_URL",
		},
		{
			name:    "malformed duration",
			env:     map[string]string{"DATABASE_URL": dbURL, "DB_TIMEOUT": "3"},
			wantErr: `missing unit in duration "3"`,
		},
		{
			name:    "unknown log level",
			env:     map[string]string{"DATABASE_URL": dbURL, "LOG_LEVEL": "verbose"},
			wantErr: `level string "verbose"`,
		},
		{
			name:    "unlimited pool",
			env:     map[string]string{"DATABASE_URL": dbURL, "DB_MAX_OPEN_CONNS": "0"},
			wantErr: "DB_MAX_OPEN_CONNS",
		},
		{
			name: "every invalid value reported",
			env: map[string]string{
				"DATABASE_URL":        dbURL,
				"DB_TIMEOUT":          "0s",
				"MAX_TRANSFER_AMOUNT": "-1",
				"MERCHANT_ID":         "0",
			},
			wantErr: "DB_TIMEOUT must be greater than 0\nMAX_TRANSFER_AMOUNT must be greater than 0\nMERCHANT_ID must be greater than 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setenv restores the original value afterwards, Unsetenv covers the "missing" case.
			t.Setenv("DATABASE_URL", "")
			if err := os.Unsetenv("DATABASE_URL"); err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadAPI()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAPILogValueHidesPassword(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{url: dbURL, want: strings.Replace(dbURL, "s3cret", "xxxxx", 1)},
		{url: "host=db user=app password=s3cret", want: "(hidden)"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", config.API{DB: config.DB{URL: tt.url}})

		if strings.Contains(buf.String(), "s3cret") {
			t.Errorf("log line leaks the password: %s", buf.String())
		}
		if !strings.Contains(buf.String(), tt.want) {
			t.Errorf("log line %s doesn't contain %q", buf.String(), tt.want)
		}
	}
}
