// Package config загружает конфигурацию сервиса из JSON-файла.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"plata-quotes-assessment/internal/exchangerates"
)

type Config struct {
	HTTP          HTTP          `json:"http"`
	Database      Database      `json:"database"`
	ExchangeRates ExchangeRates `json:"exchange_rates"`
	Worker        Worker        `json:"worker"`
}

type HTTP struct {
	Addr   string `json:"addr"`    // адрес HTTP-сервера, напр. ":8080"
	APIKey string `json:"api_key"` // ключ для X-API-Key
}

type Database struct {
	URL string `json:"url"` // DSN PostgreSQL
}

type ExchangeRates struct {
	URL       string `json:"url"`        // базовый URL провайдера
	AccessKey string `json:"access_key"` // ключ exchangeratesapi.io
}

type Worker struct {
	BatchSize    int      `json:"batch_size"`    // размер пачки заявок
	PollInterval Duration `json:"poll_interval"` // как часто проверять новые заявки
	MaxAttempts  int      `json:"max_attempts"`  // максимум попыток на заявку
	LockTimeout  Duration `json:"lock_timeout"`  // через сколько processing считается зависшей
	RateTTL      Duration `json:"rate_ttl"`      // сколько курс от провайдера считается свежим; 0 — без кеша
}

// Duration — time.Duration, который в JSON записывается строкой ("1s", "30s").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1s\": %w", err)
	}

	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}

	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Load читает конфиг из JSON-файла path. Неизвестные поля — ошибка.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if cfg.ExchangeRates.URL == "" {
		cfg.ExchangeRates.URL = exchangerates.DefaultURL
	}

	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}

	return cfg, nil
}

func (c Config) validate() error {
	var errs []error
	if c.HTTP.Addr == "" {
		errs = append(errs, errors.New("http.addr is required"))
	}
	if c.HTTP.APIKey == "" {
		errs = append(errs, errors.New("http.api_key is required"))
	}
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required"))
	}
	if c.ExchangeRates.AccessKey == "" {
		errs = append(errs, errors.New("exchange_rates.access_key is required"))
	}
	if c.Worker.BatchSize <= 0 {
		errs = append(errs, errors.New("worker.batch_size must be positive"))
	}
	if c.Worker.PollInterval <= 0 {
		errs = append(errs, errors.New("worker.poll_interval must be positive"))
	}
	if c.Worker.MaxAttempts <= 0 {
		errs = append(errs, errors.New("worker.max_attempts must be positive"))
	}
	if c.Worker.LockTimeout <= 0 {
		errs = append(errs, errors.New("worker.lock_timeout must be positive"))
	}
	if c.Worker.RateTTL < 0 {
		errs = append(errs, errors.New("worker.rate_ttl must not be negative"))
	}
	return errors.Join(errs...)
}
