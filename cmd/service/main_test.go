//go:build fun

// ИСПОЛЬЗУЕТСЯ КАК РУЧНЫЕ ТЕСТЫ
// Интеграционный тест Service с настройками из config.json: настоящий
// PostgreSQL (database.url, схема migration/init.up.sql должна быть применена),
// настоящий провайдер (exchange_rates) и воркер (worker).
//
//	go test -tags fun ./cmd/service -v
package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"plata-quotes-assessment/api"
	"plata-quotes-assessment/implementations"
	"plata-quotes-assessment/internal/config"
	"plata-quotes-assessment/internal/database"
	"plata-quotes-assessment/internal/exchangerates"
)

func TestService(t *testing.T) {
	cfg, err := config.Load("../../config.json")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db, err := database.New(ctx, cfg.Database.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(t.Output(), nil))
	svc := implementations.NewService(db, db, cfg.HTTP.APIKey, log)
	provider := exchangerates.NewClient(cfg.ExchangeRates.URL, cfg.ExchangeRates.AccessKey, nil)
	worker := implementations.NewWorker(db, provider, implementations.WorkerConfig{
		PollInterval: time.Duration(cfg.Worker.PollInterval),
		BatchSize:    cfg.Worker.BatchSize,
		MaxAttempts:  cfg.Worker.MaxAttempts,
		LockTimeout:  time.Duration(cfg.Worker.LockTimeout),
		RateTTL:      time.Duration(cfg.Worker.RateTTL),
	}, log)

	workerCtx, stopWorker := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { worker.Run(workerCtx) })
	defer func() {
		stopWorker()
		wg.Wait()
	}()

	// 1. POST /quotes/updates
	created, err := svc.CreateQuoteUpdate(ctx,
		&api.CreateUpdateRequest{Base: api.CurrencyEUR, Quote: api.CurrencyMXN},
		api.CreateQuoteUpdateParams{IdempotencyKey: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}

	t.Log(created)

	// 2. GET /quotes/updates/{id} — ждём, пока воркер обработает заявку (до 30 с).
	var u *api.QuoteUpdate
	for range 300 {
		u, err = svc.GetQuoteUpdate(ctx, api.GetQuoteUpdateParams{ID: created.UpdateID})
		if err != nil {
			t.Fatal(err)
		}
		if u.Status == api.UpdateStatusDone || u.Status == api.UpdateStatusFailed {
			break
		}
		t.Log(u)
		time.Sleep(100 * time.Millisecond)
	}
	if u.Status != api.UpdateStatusDone || u.Price.Or("") == "" {
		t.Log(u)
		t.Fatalf("update: status %s, price %q, error %q; want done with price", u.Status, u.Price.Or(""), u.Error.Or(""))
	}
	t.Log(u)

	// 3. GET /quotes/latest
	q, err := svc.GetLatestQuote(ctx, api.GetLatestQuoteParams{Base: api.CurrencyEUR, Quote: api.CurrencyMXN})
	if err != nil {
		t.Fatal(err)
	}
	if q.Price == "" {
		t.Fatal("latest: empty price")
	}
	t.Log(q)
}
