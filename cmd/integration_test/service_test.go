// Package integration — интеграционный тест Service: настоящий PostgreSQL,
// воркер и фейковый провайдер курсов.
//
// Нужен PostgreSQL с применённой схемой (migration/init.up.sql), DSN — в переменной
// QUOTES_TEST_DATABASE_URL. Без переменной тест пропускается.
//
//	QUOTES_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5435/quotes?sslmode=disable' \
//	  go test ./cmd/integration_test -v
package integration

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"plata-quotes-assessment/api"
	"plata-quotes-assessment/implementations"
	"plata-quotes-assessment/internal/database"
	"plata-quotes-assessment/internal/exchangerates"
)

const envDatabaseURL = "QUOTES_TEST_DATABASE_URL"

// TestService создаёт заявку, ждёт, пока воркер её обработает, и забирает
// курс по заявке и как последний курс пары.
func TestService(t *testing.T) {
	const price = "20.4512"
	// Время курса — текущее, чтобы курс теста был последним по паре, даже если
	// в базе уже есть курсы.
	rateTime := time.Now().UTC().Truncate(time.Second)

	db := newDatabase(t)
	log := slog.New(slog.NewTextHandler(t.Output(), nil))

	svc := implementations.NewService(db, db, "test-api-key", log)
	startWorker(t, db, fakeProvider{price: price, updatedAt: rateTime}, log)

	ctx := context.Background()

	// POST /quotes/updates
	created, err := svc.CreateQuoteUpdate(ctx,
		&api.CreateUpdateRequest{Base: api.CurrencyEUR, Quote: api.CurrencyMXN},
		api.CreateQuoteUpdateParams{IdempotencyKey: uuid.New()})
	if err != nil {
		t.Fatalf("CreateQuoteUpdate: %v", err)
	}

	// GET /quotes/updates/{id} — ждём, пока воркер обработает заявку.
	var u *api.QuoteUpdate
	deadline := time.Now().Add(10 * time.Second)
	for {
		u, err = svc.GetQuoteUpdate(ctx, api.GetQuoteUpdateParams{ID: created.UpdateID})
		if err != nil {
			t.Fatalf("GetQuoteUpdate: %v", err)
		}
		if u.Status == api.UpdateStatusDone || u.Status == api.UpdateStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("update is still %s after 10s", u.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if u.Status != api.UpdateStatusDone {
		t.Fatalf("status = %s, want done (error: %q)", u.Status, u.Error.Or(""))
	}
	if got := u.Price.Or(""); got != price {
		t.Errorf("update price = %q, want %q", got, price)
	}
	if got, _ := u.UpdatedAt.Get(); !got.Equal(rateTime) {
		t.Errorf("update updated_at = %s, want %s", got, rateTime)
	}

	// GET /quotes/latest
	q, err := svc.GetLatestQuote(ctx, api.GetLatestQuoteParams{Base: api.CurrencyEUR, Quote: api.CurrencyMXN})
	if err != nil {
		t.Fatalf("GetLatestQuote: %v", err)
	}
	if q.Price != price {
		t.Errorf("latest price = %q, want %q", q.Price, price)
	}
	if !q.UpdatedAt.Equal(rateTime) {
		t.Errorf("latest updated_at = %s, want %s", q.UpdatedAt, rateTime)
	}
}

// newDatabase подключается к базе из QUOTES_TEST_DATABASE_URL.
func newDatabase(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv(envDatabaseURL)
	if dsn == "" {
		t.Skipf("%s is not set", envDatabaseURL)
	}

	db, err := database.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	t.Cleanup(db.Close)

	return db
}

// startWorker запускает воркер и останавливает его после теста.
func startWorker(t *testing.T, store implementations.WorkerStore, provider implementations.RateProvider, log *slog.Logger) {
	t.Helper()

	worker := implementations.NewWorker(store, provider, implementations.WorkerConfig{
		PollInterval: 50 * time.Millisecond,
		BatchSize:    100,
		MaxAttempts:  3,
		LockTimeout:  time.Minute,
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		worker.Run(ctx)
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// fakeProvider отдаёт один и тот же курс для любой пары.
type fakeProvider struct {
	price     string
	updatedAt time.Time
}

func (p fakeProvider) GetRates(_ context.Context, base string, quotes ...string) (map[string]exchangerates.Rate, error) {
	rates := make(map[string]exchangerates.Rate, len(quotes))
	for _, quote := range quotes {
		rates[quote] = exchangerates.Rate{
			Base:      base,
			Quote:     quote,
			Price:     decimal.RequireFromString(p.price),
			UpdatedAt: p.updatedAt,
		}
	}
	return rates, nil
}
