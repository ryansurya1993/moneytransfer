// Package config reads each binary's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/caarlos0/env/v11"
)

// DB holds the database connection settings.
type DB struct {
	// required alone accepts a variable that is set but empty.
	URL                      string        `env:"DATABASE_URL,required,notEmpty"`
	MaxOpenConns             int           `env:"DB_MAX_OPEN_CONNS" envDefault:"20"`
	MaxIdleConns             int           `env:"DB_MAX_IDLE_CONNS" envDefault:"10"`
	ConnMaxLifetime          time.Duration `env:"DB_CONN_MAX_LIFETIME" envDefault:"30m"`
	IdleInTransactionTimeout time.Duration `env:"DB_IDLE_IN_TRANSACTION_TIMEOUT" envDefault:"10s"`
}

// HTTP holds the HTTP server settings.
type HTTP struct {
	Port              int           `env:"HTTP_PORT" envDefault:"8080"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	ReadTimeout       time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"10s"`
	WriteTimeout      time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"10s"`
	IdleTimeout       time.Duration `env:"HTTP_IDLE_TIMEOUT" envDefault:"60s"`
	ShutdownTimeout   time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"10s"`
}

// API holds the settings of the api binary.
type API struct {
	DB   DB
	HTTP HTTP

	DBTimeout         time.Duration `env:"DB_TIMEOUT" envDefault:"3s"`
	MaxTransferAmount int64         `env:"MAX_TRANSFER_AMOUNT" envDefault:"250000000"`
	MerchantID        int64         `env:"MERCHANT_ID" envDefault:"1"`
	LogLevel          slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadAPI reads the api settings from the environment.
// The error lists every missing or invalid setting, not just the first.
func LoadAPI() (API, error) {
	c, err := env.ParseAs[API]()
	if err != nil {
		return API{}, err
	}

	var errs []error
	// database/sql treats 0 as no limit.
	if c.DB.MaxOpenConns <= 0 {
		errs = append(errs, errors.New("DB_MAX_OPEN_CONNS must be greater than 0"))
	}
	if c.DBTimeout <= 0 {
		errs = append(errs, errors.New("DB_TIMEOUT must be greater than 0"))
	}
	if c.DB.IdleInTransactionTimeout <= 0 {
		errs = append(errs, errors.New("DB_IDLE_IN_TRANSACTION_TIMEOUT must be greater than 0"))
	}
	if c.MaxTransferAmount <= 0 {
		errs = append(errs, errors.New("MAX_TRANSFER_AMOUNT must be greater than 0"))
	}
	if c.MerchantID <= 0 {
		errs = append(errs, errors.New("MERCHANT_ID must be greater than 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return API{}, err
	}
	return c, nil
}

// LogValue lists the settings by variable name, with the database password hidden.
func (c API) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("DATABASE_URL", redactURL(c.DB.URL)),
		slog.Int("DB_MAX_OPEN_CONNS", c.DB.MaxOpenConns),
		slog.Int("DB_MAX_IDLE_CONNS", c.DB.MaxIdleConns),
		slog.String("DB_CONN_MAX_LIFETIME", c.DB.ConnMaxLifetime.String()),
		slog.String("DB_IDLE_IN_TRANSACTION_TIMEOUT", c.DB.IdleInTransactionTimeout.String()),
		slog.String("DB_TIMEOUT", c.DBTimeout.String()),
		slog.Int64("MAX_TRANSFER_AMOUNT", c.MaxTransferAmount),
		slog.Int64("MERCHANT_ID", c.MerchantID),
		slog.String("LOG_LEVEL", c.LogLevel.String()),
	}
	return slog.GroupValue(append(attrs, c.HTTP.logAttrs()...)...)
}

// Relay holds the settings of the relay binary.
type Relay struct {
	DB   DB
	HTTP HTTP

	RabbitMQURL          string        `env:"RABBITMQ_URL,required,notEmpty"`
	PollInterval         time.Duration `env:"RELAY_POLL_INTERVAL" envDefault:"200ms"`
	BatchSize            int           `env:"RELAY_BATCH_SIZE" envDefault:"50"`
	PublishTimeout       time.Duration `env:"RELAY_PUBLISH_TIMEOUT" envDefault:"5s"`
	StallTimeout         time.Duration `env:"RELAY_STALL_TIMEOUT" envDefault:"1m"`
	OutboxAlertAge       time.Duration `env:"OUTBOX_ALERT_AGE" envDefault:"2m"`
	DeliveryLimit        int           `env:"QUEUE_DELIVERY_LIMIT" envDefault:"5"`
	ReconcileAfter       time.Duration `env:"RECONCILE_AFTER" envDefault:"1m"`
	ReconcileMaxAttempts int           `env:"RECONCILE_MAX_ATTEMPTS" envDefault:"8"`
	LogLevel             slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadRelay reads the relay settings from the environment.
// The error lists every missing or invalid setting, not just the first.
func LoadRelay() (Relay, error) {
	c, err := env.ParseAs[Relay]()
	if err != nil {
		return Relay{}, err
	}

	var errs []error
	positive := []struct {
		name  string
		value int64
	}{
		{"DB_MAX_OPEN_CONNS", int64(c.DB.MaxOpenConns)},
		{"RELAY_POLL_INTERVAL", int64(c.PollInterval)},
		{"RELAY_BATCH_SIZE", int64(c.BatchSize)},
		{"RELAY_PUBLISH_TIMEOUT", int64(c.PublishTimeout)},
		{"OUTBOX_ALERT_AGE", int64(c.OutboxAlertAge)},
		{"QUEUE_DELIVERY_LIMIT", int64(c.DeliveryLimit)},
		{"RECONCILE_AFTER", int64(c.ReconcileAfter)},
		{"RECONCILE_MAX_ATTEMPTS", int64(c.ReconcileMaxAttempts)},
	}
	for _, p := range positive {
		if p.value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than 0", p.name))
		}
	}
	// One failing publish blocks a round for up to the publish timeout; that is not a stall.
	if c.StallTimeout <= c.PublishTimeout {
		errs = append(errs, errors.New("RELAY_STALL_TIMEOUT must be longer than RELAY_PUBLISH_TIMEOUT"))
	}
	// Otherwise Postgres would end the relay's transaction while it waits for a slow confirm.
	if c.DB.IdleInTransactionTimeout <= c.PublishTimeout {
		errs = append(errs, errors.New("DB_IDLE_IN_TRANSACTION_TIMEOUT must be longer than RELAY_PUBLISH_TIMEOUT"))
	}
	if err := errors.Join(errs...); err != nil {
		return Relay{}, err
	}
	return c, nil
}

// LogValue lists the settings by variable name, with passwords hidden.
func (c Relay) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("DATABASE_URL", redactURL(c.DB.URL)),
		slog.Int("DB_MAX_OPEN_CONNS", c.DB.MaxOpenConns),
		slog.Int("DB_MAX_IDLE_CONNS", c.DB.MaxIdleConns),
		slog.String("DB_CONN_MAX_LIFETIME", c.DB.ConnMaxLifetime.String()),
		slog.String("DB_IDLE_IN_TRANSACTION_TIMEOUT", c.DB.IdleInTransactionTimeout.String()),
		slog.String("RABBITMQ_URL", redactURL(c.RabbitMQURL)),
		slog.String("RELAY_POLL_INTERVAL", c.PollInterval.String()),
		slog.Int("RELAY_BATCH_SIZE", c.BatchSize),
		slog.String("RELAY_PUBLISH_TIMEOUT", c.PublishTimeout.String()),
		slog.String("RELAY_STALL_TIMEOUT", c.StallTimeout.String()),
		slog.String("OUTBOX_ALERT_AGE", c.OutboxAlertAge.String()),
		slog.Int("QUEUE_DELIVERY_LIMIT", c.DeliveryLimit),
		slog.String("RECONCILE_AFTER", c.ReconcileAfter.String()),
		slog.Int("RECONCILE_MAX_ATTEMPTS", c.ReconcileMaxAttempts),
		slog.String("LOG_LEVEL", c.LogLevel.String()),
	}
	return slog.GroupValue(append(attrs, c.HTTP.logAttrs()...)...)
}

// LedgerAdmin holds the settings of the relay's command for resolving ledger findings.
// It needs only the database, so it runs while the broker is down.
type LedgerAdmin struct {
	DB DB
}

// LoadLedgerAdmin reads the settings of the ledger command from the environment.
func LoadLedgerAdmin() (LedgerAdmin, error) {
	return env.ParseAs[LedgerAdmin]()
}

// Worker holds the settings of the worker binary.
type Worker struct {
	DB   DB
	HTTP HTTP

	DBTimeout           time.Duration `env:"DB_TIMEOUT" envDefault:"3s"`
	RabbitMQURL         string        `env:"RABBITMQ_URL,required,notEmpty"`
	DeliveryLimit       int           `env:"QUEUE_DELIVERY_LIMIT" envDefault:"5"`
	Concurrency         int           `env:"WORKER_CONCURRENCY" envDefault:"20"`
	ShutdownTimeout     time.Duration `env:"WORKER_SHUTDOWN_TIMEOUT" envDefault:"20s"`
	ProviderURL         string        `env:"PROVIDER_URL,required,notEmpty"`
	ProviderTimeout     time.Duration `env:"PROVIDER_TIMEOUT" envDefault:"3s"`
	ProviderMaxAttempts int           `env:"PROVIDER_MAX_ATTEMPTS" envDefault:"3"`
	ProviderBackoffBase time.Duration `env:"PROVIDER_BACKOFF_BASE" envDefault:"1s"`
	ProviderBackoffMax  time.Duration `env:"PROVIDER_BACKOFF_MAX" envDefault:"4s"`
	LogLevel            slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadWorker reads the worker settings from the environment.
// The error lists every missing or invalid setting, not just the first.
func LoadWorker() (Worker, error) {
	c, err := env.ParseAs[Worker]()
	if err != nil {
		return Worker{}, err
	}

	var errs []error
	positive := []struct {
		name  string
		value int64
	}{
		{"DB_MAX_OPEN_CONNS", int64(c.DB.MaxOpenConns)},
		{"DB_IDLE_IN_TRANSACTION_TIMEOUT", int64(c.DB.IdleInTransactionTimeout)},
		{"DB_TIMEOUT", int64(c.DBTimeout)},
		{"QUEUE_DELIVERY_LIMIT", int64(c.DeliveryLimit)},
		{"WORKER_CONCURRENCY", int64(c.Concurrency)},
		{"PROVIDER_TIMEOUT", int64(c.ProviderTimeout)},
		{"PROVIDER_MAX_ATTEMPTS", int64(c.ProviderMaxAttempts)},
		{"PROVIDER_BACKOFF_BASE", int64(c.ProviderBackoffBase)},
	}
	for _, p := range positive {
		if p.value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than 0", p.name))
		}
	}
	if c.ProviderBackoffMax < c.ProviderBackoffBase {
		errs = append(errs, errors.New("PROVIDER_BACKOFF_MAX must not be less than PROVIDER_BACKOFF_BASE"))
	}
	// A transfer cut off by shutdown is safe, but it waits until its message comes back.
	// A normal shutdown should let a full round of provider retries finish.
	if c.ShutdownTimeout <= c.maxPayoutTime() {
		errs = append(errs, fmt.Errorf("WORKER_SHUTDOWN_TIMEOUT must be longer than the longest payout round (%s)", c.maxPayoutTime()))
	}
	if err := errors.Join(errs...); err != nil {
		return Worker{}, err
	}
	return c, nil
}

// maxPayoutTime is the longest a payout round can take.
// That is every attempt timing out, with the longest wait between attempts.
func (c Worker) maxPayoutTime() time.Duration {
	attempts := time.Duration(c.ProviderMaxAttempts)
	return attempts*c.ProviderTimeout + (attempts-1)*c.ProviderBackoffMax
}

// LogValue lists the settings by variable name, with passwords hidden.
func (c Worker) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("DATABASE_URL", redactURL(c.DB.URL)),
		slog.Int("DB_MAX_OPEN_CONNS", c.DB.MaxOpenConns),
		slog.Int("DB_MAX_IDLE_CONNS", c.DB.MaxIdleConns),
		slog.String("DB_CONN_MAX_LIFETIME", c.DB.ConnMaxLifetime.String()),
		slog.String("DB_IDLE_IN_TRANSACTION_TIMEOUT", c.DB.IdleInTransactionTimeout.String()),
		slog.String("DB_TIMEOUT", c.DBTimeout.String()),
		slog.String("RABBITMQ_URL", redactURL(c.RabbitMQURL)),
		slog.Int("QUEUE_DELIVERY_LIMIT", c.DeliveryLimit),
		slog.Int("WORKER_CONCURRENCY", c.Concurrency),
		slog.String("WORKER_SHUTDOWN_TIMEOUT", c.ShutdownTimeout.String()),
		slog.String("PROVIDER_URL", redactURL(c.ProviderURL)),
		slog.String("PROVIDER_TIMEOUT", c.ProviderTimeout.String()),
		slog.Int("PROVIDER_MAX_ATTEMPTS", c.ProviderMaxAttempts),
		slog.String("PROVIDER_BACKOFF_BASE", c.ProviderBackoffBase.String()),
		slog.String("PROVIDER_BACKOFF_MAX", c.ProviderBackoffMax.String()),
		slog.String("LOG_LEVEL", c.LogLevel.String()),
	}
	return slog.GroupValue(append(attrs, c.HTTP.logAttrs()...)...)
}

// MockProvider holds the settings of the fake payout provider.
type MockProvider struct {
	HTTP HTTP

	MinDelay           time.Duration `env:"MOCK_MIN_DELAY" envDefault:"200ms"`
	MaxDelay           time.Duration `env:"MOCK_MAX_DELAY" envDefault:"2s"`
	FailurePercent     int           `env:"MOCK_FAILURE_PERCENT" envDefault:"10"`
	TimeoutPercent     int           `env:"MOCK_TIMEOUT_PERCENT" envDefault:"5"`
	UnavailablePercent int           `env:"MOCK_UNAVAILABLE_PERCENT" envDefault:"5"`
	// HangDuration is how long a paid-but-timed-out call stalls. It must exceed the caller's timeout.
	HangDuration time.Duration `env:"MOCK_HANG_DURATION" envDefault:"8s"`
	LogLevel     slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadMockProvider reads the mock provider settings from the environment.
// The error lists every missing or invalid setting, not just the first.
func LoadMockProvider() (MockProvider, error) {
	c, err := env.ParseAs[MockProvider]()
	if err != nil {
		return MockProvider{}, err
	}

	var errs []error
	percents := []struct {
		name  string
		value int
	}{
		{"MOCK_FAILURE_PERCENT", c.FailurePercent},
		{"MOCK_TIMEOUT_PERCENT", c.TimeoutPercent},
		{"MOCK_UNAVAILABLE_PERCENT", c.UnavailablePercent},
	}
	for _, p := range percents {
		if p.value < 0 || p.value > 100 {
			errs = append(errs, fmt.Errorf("%s must be between 0 and 100", p.name))
		}
	}
	if c.FailurePercent+c.TimeoutPercent+c.UnavailablePercent > 100 {
		errs = append(errs, errors.New("MOCK_FAILURE_PERCENT, MOCK_TIMEOUT_PERCENT and MOCK_UNAVAILABLE_PERCENT must add up to at most 100"))
	}
	if c.MinDelay < 0 || c.MinDelay > c.MaxDelay {
		errs = append(errs, errors.New("MOCK_MIN_DELAY must be between 0 and MOCK_MAX_DELAY"))
	}
	if c.HangDuration <= 0 {
		errs = append(errs, errors.New("MOCK_HANG_DURATION must be greater than 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return MockProvider{}, err
	}
	return c, nil
}

// LogValue lists the settings by variable name.
func (c MockProvider) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("MOCK_MIN_DELAY", c.MinDelay.String()),
		slog.String("MOCK_MAX_DELAY", c.MaxDelay.String()),
		slog.Int("MOCK_FAILURE_PERCENT", c.FailurePercent),
		slog.Int("MOCK_TIMEOUT_PERCENT", c.TimeoutPercent),
		slog.Int("MOCK_UNAVAILABLE_PERCENT", c.UnavailablePercent),
		slog.String("MOCK_HANG_DURATION", c.HangDuration.String()),
		slog.String("LOG_LEVEL", c.LogLevel.String()),
	}
	return slog.GroupValue(append(attrs, c.HTTP.logAttrs()...)...)
}

// LoadTest holds the settings of the loadtest binary.
type LoadTest struct {
	DB DB

	TargetURL        string        `env:"LOADTEST_TARGET_URL" envDefault:"http://haproxy:8080"`
	ProviderURL      string        `env:"LOADTEST_PROVIDER_URL" envDefault:"http://mockprovider:8080"`
	Rate             int           `env:"LOADTEST_RATE" envDefault:"20"`
	Duration         time.Duration `env:"LOADTEST_DURATION" envDefault:"30s"`
	Concurrency      int           `env:"LOADTEST_CONCURRENCY" envDefault:"20"`
	DuplicatePercent int           `env:"LOADTEST_DUPLICATE_PERCENT" envDefault:"10"`
	MagicPercent     int           `env:"LOADTEST_MAGIC_PERCENT" envDefault:"5"`
	RequestTimeout   time.Duration `env:"LOADTEST_REQUEST_TIMEOUT" envDefault:"5s"`
	DrainTimeout     time.Duration `env:"LOADTEST_DRAIN_TIMEOUT" envDefault:"90s"`
	MinAmount        int64         `env:"LOADTEST_MIN_AMOUNT" envDefault:"50000"`
	MaxAmount        int64         `env:"LOADTEST_MAX_AMOUNT" envDefault:"5000000"`
	MerchantID       int64         `env:"MERCHANT_ID" envDefault:"1"`
}

// LoadLoadTest reads the load test settings from the environment.
// The error lists every missing or invalid setting, not just the first.
func LoadLoadTest() (LoadTest, error) {
	c, err := env.ParseAs[LoadTest]()
	if err != nil {
		return LoadTest{}, err
	}

	var errs []error
	urls := []struct{ name, value string }{
		{"LOADTEST_TARGET_URL", c.TargetURL},
		{"LOADTEST_PROVIDER_URL", c.ProviderURL},
	}
	for _, u := range urls {
		if parsed, err := url.Parse(u.value); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an absolute URL", u.name))
		}
	}
	positive := []struct {
		name  string
		value int64
	}{
		{"LOADTEST_RATE", int64(c.Rate)},
		{"LOADTEST_DURATION", int64(c.Duration)},
		{"LOADTEST_CONCURRENCY", int64(c.Concurrency)},
		{"LOADTEST_REQUEST_TIMEOUT", int64(c.RequestTimeout)},
		{"LOADTEST_DRAIN_TIMEOUT", int64(c.DrainTimeout)},
		{"LOADTEST_MIN_AMOUNT", c.MinAmount},
		{"MERCHANT_ID", c.MerchantID},
	}
	for _, p := range positive {
		if p.value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than 0", p.name))
		}
	}
	percents := []struct {
		name  string
		value int
	}{
		{"LOADTEST_DUPLICATE_PERCENT", c.DuplicatePercent},
		{"LOADTEST_MAGIC_PERCENT", c.MagicPercent},
	}
	for _, p := range percents {
		if p.value < 0 || p.value > 100 {
			errs = append(errs, fmt.Errorf("%s must be between 0 and 100", p.name))
		}
	}
	if c.MaxAmount < c.MinAmount {
		errs = append(errs, errors.New("LOADTEST_MAX_AMOUNT must not be less than LOADTEST_MIN_AMOUNT"))
	}
	if err := errors.Join(errs...); err != nil {
		return LoadTest{}, err
	}
	return c, nil
}

func (h HTTP) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("HTTP_PORT", h.Port),
		slog.String("HTTP_READ_HEADER_TIMEOUT", h.ReadHeaderTimeout.String()),
		slog.String("HTTP_READ_TIMEOUT", h.ReadTimeout.String()),
		slog.String("HTTP_WRITE_TIMEOUT", h.WriteTimeout.String()),
		slog.String("HTTP_IDLE_TIMEOUT", h.IdleTimeout.String()),
		slog.String("HTTP_SHUTDOWN_TIMEOUT", h.ShutdownTimeout.String()),
	}
}

// redactURL hides the password in a URL. Anything that doesn't parse as a URL with a scheme
// (a key=value connection string, say) is hidden completely, since it may contain one too.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return "(hidden)"
	}
	return u.Redacted()
}
