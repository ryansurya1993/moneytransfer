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
	URL             string        `env:"DATABASE_URL,required,notEmpty"`
	MaxOpenConns    int           `env:"DB_MAX_OPEN_CONNS" envDefault:"20"`
	MaxIdleConns    int           `env:"DB_MAX_IDLE_CONNS" envDefault:"10"`
	ConnMaxLifetime time.Duration `env:"DB_CONN_MAX_LIFETIME" envDefault:"30m"`
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
		slog.String("DB_TIMEOUT", c.DBTimeout.String()),
		slog.Int64("MAX_TRANSFER_AMOUNT", c.MaxTransferAmount),
		slog.Int64("MERCHANT_ID", c.MerchantID),
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
