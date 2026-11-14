// Package config reads each binary's settings from environment variables.
package config

import (
	"errors"
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
	return slog.GroupValue(
		slog.String("DATABASE_URL", redactURL(c.DB.URL)),
		slog.Int("DB_MAX_OPEN_CONNS", c.DB.MaxOpenConns),
		slog.Int("DB_MAX_IDLE_CONNS", c.DB.MaxIdleConns),
		slog.String("DB_CONN_MAX_LIFETIME", c.DB.ConnMaxLifetime.String()),
		slog.String("DB_TIMEOUT", c.DBTimeout.String()),
		slog.Int("HTTP_PORT", c.HTTP.Port),
		slog.String("HTTP_READ_HEADER_TIMEOUT", c.HTTP.ReadHeaderTimeout.String()),
		slog.String("HTTP_READ_TIMEOUT", c.HTTP.ReadTimeout.String()),
		slog.String("HTTP_WRITE_TIMEOUT", c.HTTP.WriteTimeout.String()),
		slog.String("HTTP_IDLE_TIMEOUT", c.HTTP.IdleTimeout.String()),
		slog.String("HTTP_SHUTDOWN_TIMEOUT", c.HTTP.ShutdownTimeout.String()),
		slog.Int64("MAX_TRANSFER_AMOUNT", c.MaxTransferAmount),
		slog.Int64("MERCHANT_ID", c.MerchantID),
		slog.String("LOG_LEVEL", c.LogLevel.String()),
	)
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
