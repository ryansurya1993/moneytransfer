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

const amqpURL = "amqp://app:s3cret@rabbitmq:5672/" //nolint:gosec // fake test credentials

func TestLoadRelay(t *testing.T) {
	t.Setenv("DATABASE_URL", dbURL)
	t.Setenv("RABBITMQ_URL", amqpURL)

	c, err := config.LoadRelay()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 200*time.Millisecond || c.BatchSize != 50 || c.PublishTimeout != 5*time.Second {
		t.Errorf("poll %v batch %d publish %v, want 200ms 50 5s", c.PollInterval, c.BatchSize, c.PublishTimeout)
	}
	if c.StallTimeout != time.Minute || c.OutboxAlertAge != 2*time.Minute || c.DeliveryLimit != 5 {
		t.Errorf("stall %v alert %v limit %d, want 1m 2m 5", c.StallTimeout, c.OutboxAlertAge, c.DeliveryLimit)
	}
	if c.ReconcileAfter != time.Minute || c.ReconcileMaxAttempts != 8 {
		t.Errorf("reconcile after %v, %d tries; want 1m, 8", c.ReconcileAfter, c.ReconcileMaxAttempts)
	}
}

func TestLoadRelayErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "rabbitmq url missing", env: map[string]string{"RABBITMQ_URL": ""}, wantErr: "RABBITMQ_URL"},
		{name: "zero batch", env: map[string]string{"RELAY_BATCH_SIZE": "0"}, wantErr: "RELAY_BATCH_SIZE must be greater than 0"},
		{name: "zero delivery limit", env: map[string]string{"QUEUE_DELIVERY_LIMIT": "0"}, wantErr: "QUEUE_DELIVERY_LIMIT must be greater than 0"},
		{name: "zero reconcile gap", env: map[string]string{"RECONCILE_AFTER": "0s"}, wantErr: "RECONCILE_AFTER must be greater than 0"},
		{name: "zero reconcile tries", env: map[string]string{"RECONCILE_MAX_ATTEMPTS": "0"}, wantErr: "RECONCILE_MAX_ATTEMPTS must be greater than 0"},
		{
			name:    "stall shorter than one publish",
			env:     map[string]string{"RELAY_STALL_TIMEOUT": "5s", "RELAY_PUBLISH_TIMEOUT": "5s"},
			wantErr: "RELAY_STALL_TIMEOUT must be longer than RELAY_PUBLISH_TIMEOUT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", dbURL)
			t.Setenv("RABBITMQ_URL", amqpURL)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadRelay()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRelayLogValueHidesPasswords(t *testing.T) {
	var buf bytes.Buffer
	c := config.Relay{DB: config.DB{URL: dbURL}, RabbitMQURL: amqpURL}
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)

	if strings.Contains(buf.String(), "s3cret") {
		t.Errorf("log line leaks a password: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "amqp://app:xxxxx@rabbitmq:5672/") {
		t.Errorf("log line %s doesn't contain the redacted RabbitMQ URL", buf.String())
	}
}

func TestIdleInTransactionTimeout(t *testing.T) {
	tests := []struct {
		name    string
		load    func() error
		env     map[string]string
		wantErr string
	}{
		{
			name:    "api rejects zero",
			load:    func() error { _, err := config.LoadAPI(); return err },
			env:     map[string]string{"DB_IDLE_IN_TRANSACTION_TIMEOUT": "0s"},
			wantErr: "DB_IDLE_IN_TRANSACTION_TIMEOUT must be greater than 0",
		},
		{
			name:    "relay needs it above the publish timeout",
			load:    func() error { _, err := config.LoadRelay(); return err },
			env:     map[string]string{"DB_IDLE_IN_TRANSACTION_TIMEOUT": "5s", "RELAY_PUBLISH_TIMEOUT": "5s"},
			wantErr: "DB_IDLE_IN_TRANSACTION_TIMEOUT must be longer than RELAY_PUBLISH_TIMEOUT",
		},
		{
			name: "defaults are consistent",
			load: func() error { _, err := config.LoadRelay(); return err },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", dbURL)
			t.Setenv("RABBITMQ_URL", amqpURL)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			err := tt.load()
			if tt.wantErr == "" && err != nil {
				t.Errorf("got error %v, want none", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func setWorkerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", dbURL)
	t.Setenv("RABBITMQ_URL", amqpURL)
	t.Setenv("PROVIDER_URL", "http://mockprovider:8080")
}

func TestLoadWorker(t *testing.T) {
	setWorkerEnv(t)

	c, err := config.LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 20 || c.ShutdownTimeout != 20*time.Second || c.ProviderMaxAttempts != 3 {
		t.Errorf("concurrency %d, shutdown %v, attempts %d; want 20, 20s, 3", c.Concurrency, c.ShutdownTimeout, c.ProviderMaxAttempts)
	}
	if c.ProviderTimeout != 3*time.Second || c.ProviderBackoffBase != time.Second || c.ProviderBackoffMax != 4*time.Second {
		t.Errorf("provider timeout %v, backoff %v-%v; want 3s, 1s-4s", c.ProviderTimeout, c.ProviderBackoffBase, c.ProviderBackoffMax)
	}
}

func TestLoadWorkerErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "provider url missing", env: map[string]string{"PROVIDER_URL": ""}, wantErr: "PROVIDER_URL"},
		{name: "no concurrency", env: map[string]string{"WORKER_CONCURRENCY": "0"}, wantErr: "WORKER_CONCURRENCY must be greater than 0"},
		{name: "backoff max below base", env: map[string]string{"PROVIDER_BACKOFF_MAX": "500ms"}, wantErr: "PROVIDER_BACKOFF_MAX must not be less than PROVIDER_BACKOFF_BASE"},
		{
			// 3 attempts x 3s + 2 waits x 4s = 17s.
			name:    "shutdown shorter than a payout round",
			env:     map[string]string{"WORKER_SHUTDOWN_TIMEOUT": "17s"},
			wantErr: "WORKER_SHUTDOWN_TIMEOUT must be longer than the longest payout round (17s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWorkerEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadWorker()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWorkerLogValueHidesPasswords(t *testing.T) {
	var buf bytes.Buffer
	c := config.Worker{DB: config.DB{URL: dbURL}, RabbitMQURL: amqpURL, ProviderURL: "http://mockprovider:8080"}
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)

	if strings.Contains(buf.String(), "s3cret") {
		t.Errorf("log line leaks a password: %s", buf.String())
	}
}

func TestLoadLoadTest(t *testing.T) {
	t.Setenv("DATABASE_URL", dbURL)

	c, err := config.LoadLoadTest()
	if err != nil {
		t.Fatal(err)
	}
	if c.TargetURL != "http://haproxy:8080" || c.Rate != 20 || c.Duration != 30*time.Second || c.Concurrency != 20 {
		t.Errorf("target %q, rate %d, duration %v, concurrency %d; want http://haproxy:8080, 20, 30s, 20",
			c.TargetURL, c.Rate, c.Duration, c.Concurrency)
	}
	if c.MinAmount != 50_000 || c.MaxAmount != 5_000_000 {
		t.Errorf("amounts %d-%d, want 50000-5000000", c.MinAmount, c.MaxAmount)
	}
	if c.DuplicatePercent != 10 || c.MagicPercent != 5 || c.DrainTimeout != 90*time.Second {
		t.Errorf("duplicates %d%%, magic %d%%, drain %v; want 10%%, 5%%, 90s", c.DuplicatePercent, c.MagicPercent, c.DrainTimeout)
	}
}

func TestLoadLoadTestErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "database url missing", env: map[string]string{"DATABASE_URL": ""}, wantErr: "DATABASE_URL"},
		{name: "relative target", env: map[string]string{"LOADTEST_TARGET_URL": "api:8080"}, wantErr: "LOADTEST_TARGET_URL must be an absolute URL"},
		{name: "no rate", env: map[string]string{"LOADTEST_RATE": "0"}, wantErr: "LOADTEST_RATE must be greater than 0"},
		{name: "duplicates over 100", env: map[string]string{"LOADTEST_DUPLICATE_PERCENT": "101"}, wantErr: "LOADTEST_DUPLICATE_PERCENT must be between 0 and 100"},
		{name: "negative magic", env: map[string]string{"LOADTEST_MAGIC_PERCENT": "-1"}, wantErr: "LOADTEST_MAGIC_PERCENT must be between 0 and 100"},
		{name: "max amount below min", env: map[string]string{"LOADTEST_MIN_AMOUNT": "100", "LOADTEST_MAX_AMOUNT": "99"}, wantErr: "LOADTEST_MAX_AMOUNT must not be less than LOADTEST_MIN_AMOUNT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", dbURL)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := config.LoadLoadTest()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
