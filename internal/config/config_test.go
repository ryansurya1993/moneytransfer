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

func TestLoadMockProvider(t *testing.T) {
	c, err := config.LoadMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	if c.MinDelay != 200*time.Millisecond || c.MaxDelay != 2*time.Second || c.HangDuration != 8*time.Second {
		t.Errorf("delays = %v-%v hang %v, want 200ms-2s hang 8s", c.MinDelay, c.MaxDelay, c.HangDuration)
	}
	if c.FailurePercent != 10 || c.TimeoutPercent != 5 || c.UnavailablePercent != 5 {
		t.Errorf("percents = %d/%d/%d, want 10/5/5", c.FailurePercent, c.TimeoutPercent, c.UnavailablePercent)
	}
}

func TestLoadMockProviderErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "percent out of range",
			env:     map[string]string{"MOCK_FAILURE_PERCENT": "101", "MOCK_TIMEOUT_PERCENT": "0", "MOCK_UNAVAILABLE_PERCENT": "0"},
			wantErr: "MOCK_FAILURE_PERCENT must be between 0 and 100",
		},
		{
			name:    "percents above 100 together",
			env:     map[string]string{"MOCK_FAILURE_PERCENT": "60", "MOCK_TIMEOUT_PERCENT": "30", "MOCK_UNAVAILABLE_PERCENT": "20"},
			wantErr: "must add up to at most 100",
		},
		{
			name:    "min delay above max",
			env:     map[string]string{"MOCK_MIN_DELAY": "3s", "MOCK_MAX_DELAY": "1s"},
			wantErr: "MOCK_MIN_DELAY must be between 0 and MOCK_MAX_DELAY",
		},
		{
			name:    "no hang",
			env:     map[string]string{"MOCK_HANG_DURATION": "0s"},
			wantErr: "MOCK_HANG_DURATION must be greater than 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadMockProvider()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMockProviderLogValue(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", config.MockProvider{FailurePercent: 10, HTTP: config.HTTP{Port: 8080}})

	for _, want := range []string{`"MOCK_FAILURE_PERCENT":10`, `"HTTP_PORT":8080`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log line %s doesn't contain %s", buf.String(), want)
		}
	}
}
