//go:build integration

// Интеграционные тесты Service и воркера. PostgreSQL и API-ключ берутся
// из config.json (схема migration/init.up.sql должна быть применена),
// провайдер курсов — фейковый, чтобы проверять точные цены и ошибки.
//
// Пока идут тесты, на той же базе не должен работать сервис: его воркер
// может забрать заявки тестов.
//
//	go test -tags integration ./cmd/test -v
package test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"plata-quotes-assessment/api"
	"plata-quotes-assessment/implementations"
	"plata-quotes-assessment/internal/config"
	"plata-quotes-assessment/internal/database"
	"plata-quotes-assessment/internal/exchangerates"
)

const (
	configPath  = "../../config.json"
	waitTimeout = 10 * time.Second
)

// Заявка обрабатывается воркером и отдаётся по id с ценой и временем курса.
func TestQuoteUpdateDone(t *testing.T) {
	svc, db, _ := setup(t)
	rateTime := now()
	provider := &fakeProvider{price: "20.4512", updatedAt: rateTime}
	startWorker(t, db, provider, workerConfig())

	id := createUpdate(t, svc, uuid.New(), api.CurrencyEUR, api.CurrencyMXN)
	u := waitFinished(t, svc, id)

	if u.Status != api.UpdateStatusDone {
		t.Fatalf("status = %s, want done (error %q)", u.Status, u.Error.Or(""))
	}
	if u.UpdateID != id || u.Base != api.CurrencyEUR || u.Quote != api.CurrencyMXN {
		t.Errorf("update = %s %s/%s, want %s EUR/MXN", u.UpdateID, u.Base, u.Quote, id)
	}
	if got := u.Price.Or(""); got != "20.4512" {
		t.Errorf("price = %q, want 20.4512", got)
	}
	if got, _ := u.UpdatedAt.Get(); !got.Equal(rateTime) {
		t.Errorf("updated_at = %s, want %s", got, rateTime)
	}
	if u.Error.IsSet() {
		t.Errorf("error = %q, want none", u.Error.Value)
	}
}

// Последний курс пары — самый новый по времени курса; с at — последний
// на этот момент или раньше.
func TestGetLatestQuote(t *testing.T) {
	svc, db, _ := setup(t)
	ctx := context.Background()
	older, newer := now().Add(-10*time.Second), now()
	provider := &fakeProvider{price: "18.5", updatedAt: older}
	startWorker(t, db, provider, workerConfig())

	requireDone(t, svc, createUpdate(t, svc, uuid.New(), api.CurrencyUSD, api.CurrencyMXN))
	provider.set("18.75", newer)
	requireDone(t, svc, createUpdate(t, svc, uuid.New(), api.CurrencyUSD, api.CurrencyMXN))

	tests := []struct {
		name      string
		at        api.OptDateTime
		wantPrice string
		wantTime  time.Time
	}{
		{"latest", api.OptDateTime{}, "18.75", newer},
		{"at newer rate time", api.NewOptDateTime(newer), "18.75", newer},
		{"at between rates", api.NewOptDateTime(older.Add(time.Second)), "18.5", older},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := svc.GetLatestQuote(ctx, api.GetLatestQuoteParams{
				Base: api.CurrencyUSD, Quote: api.CurrencyMXN, At: tt.at,
			})
			if err != nil {
				t.Fatalf("GetLatestQuote: %v", err)
			}
			if q.Base != api.CurrencyUSD || q.Quote != api.CurrencyMXN {
				t.Errorf("pair = %s/%s, want USD/MXN", q.Base, q.Quote)
			}
			if q.Price != tt.wantPrice || !q.UpdatedAt.Equal(tt.wantTime) {
				t.Errorf("quote = %s at %s, want %s at %s", q.Price, q.UpdatedAt, tt.wantPrice, tt.wantTime)
			}
		})
	}
}

// Повтор с тем же Idempotency-Key и парой возвращает ту же заявку,
// с другой парой — 409.
func TestIdempotency(t *testing.T) {
	svc, db, _ := setup(t)
	startWorker(t, db, &fakeProvider{price: "1.0832", updatedAt: now()}, workerConfig())
	key := uuid.New()

	first := createUpdate(t, svc, key, api.CurrencyEUR, api.CurrencyUSD)
	second := createUpdate(t, svc, key, api.CurrencyEUR, api.CurrencyUSD)
	if first != second {
		t.Errorf("repeated request created new update %s, want %s", second, first)
	}

	_, err := svc.CreateQuoteUpdate(context.Background(),
		&api.CreateUpdateRequest{Base: api.CurrencyUSD, Quote: api.CurrencyEUR},
		api.CreateQuoteUpdateParams{IdempotencyKey: key})
	requireStatus(t, svc, err, http.StatusConflict)

	requireDone(t, svc, first)
}

// Некорректные запросы: base == quote — 400, неизвестная заявка — 404.
func TestValidation(t *testing.T) {
	svc, _, _ := setup(t)
	ctx := context.Background()

	_, err := svc.CreateQuoteUpdate(ctx,
		&api.CreateUpdateRequest{Base: api.CurrencyUSD, Quote: api.CurrencyUSD},
		api.CreateQuoteUpdateParams{IdempotencyKey: uuid.New()})
	requireStatus(t, svc, err, http.StatusBadRequest)

	_, err = svc.GetLatestQuote(ctx, api.GetLatestQuoteParams{Base: api.CurrencyUSD, Quote: api.CurrencyUSD})
	requireStatus(t, svc, err, http.StatusBadRequest)

	_, err = svc.GetQuoteUpdate(ctx, api.GetQuoteUpdateParams{ID: uuid.New()})
	requireStatus(t, svc, err, http.StatusNotFound)
}

// Проверка X-API-Key: ключ из конфига принимается, другой — нет.
func TestAPIKey(t *testing.T) {
	svc, _, cfg := setup(t)
	ctx := context.Background()

	if _, err := svc.HandleApiKeyAuth(ctx, api.GetQuoteUpdateOperation, api.ApiKeyAuth{APIKey: cfg.HTTP.APIKey}); err != nil {
		t.Errorf("valid key rejected: %v", err)
	}
	if _, err := svc.HandleApiKeyAuth(ctx, api.GetQuoteUpdateOperation, api.ApiKeyAuth{APIKey: cfg.HTTP.APIKey + "-wrong"}); err == nil {
		t.Error("wrong key accepted")
	}
}

// Ошибка провайдера: заявка повторяется и после MaxAttempts попыток
// переходит в failed с текстом ошибки.
func TestProviderErrorFailsUpdate(t *testing.T) {
	svc, db, _ := setup(t)
	errProvider := errors.New("provider unavailable")
	provider := &fakeProvider{err: errProvider}
	cfg := workerConfig()
	cfg.MaxAttempts = 2
	startWorker(t, db, provider, cfg)

	u := waitFinished(t, svc, createUpdate(t, svc, uuid.New(), api.CurrencyMXN, api.CurrencyEUR))

	if u.Status != api.UpdateStatusFailed {
		t.Fatalf("status = %s, want failed", u.Status)
	}
	if got := u.Error.Or(""); got != errProvider.Error() {
		t.Errorf("error = %q, want %q", got, errProvider)
	}
	if u.Price.IsSet() || u.UpdatedAt.IsSet() {
		t.Errorf("failed update has price %q, updated_at %v", u.Price.Or(""), u.UpdatedAt)
	}
	if calls := provider.callsFor("MXN"); calls != cfg.MaxAttempts {
		t.Errorf("provider calls = %d, want %d (max attempts)", calls, cfg.MaxAttempts)
	}
}

// Заявки с одной base из одной пачки закрываются одним запросом к провайдеру.
func TestGroupingByBase(t *testing.T) {
	svc, db, _ := setup(t)
	provider := &fakeProvider{price: "1.5", updatedAt: now()}

	// Обе заявки создаются до запуска воркера, чтобы попасть в одну пачку.
	toMXN := createUpdate(t, svc, uuid.New(), api.CurrencyEUR, api.CurrencyMXN)
	toUSD := createUpdate(t, svc, uuid.New(), api.CurrencyEUR, api.CurrencyUSD)
	startWorker(t, db, provider, workerConfig())

	requireDone(t, svc, toMXN)
	requireDone(t, svc, toUSD)
	if calls := provider.callsFor("EUR"); calls != 1 {
		t.Errorf("provider calls for EUR = %d, want 1", calls)
	}
}

// С rate_ttl повторная заявка по паре закрывается свежим курсом из БД
// без запроса к провайдеру.
func TestRateCache(t *testing.T) {
	svc, db, _ := setup(t)
	provider := &fakeProvider{price: "0.9231", updatedAt: now()}
	cfg := workerConfig()
	cfg.RateTTL = time.Minute
	startWorker(t, db, provider, cfg)

	first := requireDone(t, svc, createUpdate(t, svc, uuid.New(), api.CurrencyUSD, api.CurrencyEUR))
	callsAfterFirst := provider.callsFor("USD")

	provider.set("0.95", now().Add(time.Second)) // провайдер вернул бы другой курс
	second := requireDone(t, svc, createUpdate(t, svc, uuid.New(), api.CurrencyUSD, api.CurrencyEUR))

	if calls := provider.callsFor("USD"); calls != callsAfterFirst {
		t.Errorf("provider called %d more times, want 0 (rate from cache)", calls-callsAfterFirst)
	}
	if second.Price.Or("") != first.Price.Or("") {
		t.Errorf("second price = %q, want cached %q", second.Price.Or(""), first.Price.Or(""))
	}
}

// Заявка, зависшая в processing дольше lock_timeout, возвращается в очередь
// и обрабатывается заново.
func TestStuckUpdateIsReleased(t *testing.T) {
	svc, db, _ := setup(t)
	release := make(chan struct{})
	provider := &fakeProvider{price: "17.2", updatedAt: now(), block: release}
	cfg := workerConfig()
	cfg.LockTimeout = 200 * time.Millisecond

	// Первый воркер забирает заявку и «зависает» на запросе к провайдеру.
	startWorker(t, db, provider, cfg)
	t.Cleanup(func() { close(release) }) // отпускаем до остановки первого воркера

	id := createUpdate(t, svc, uuid.New(), api.CurrencyMXN, api.CurrencyUSD)
	waitFor(t, "first worker calls provider", func() bool { return provider.callsFor("MXN") >= 1 })

	// Второй воркер возвращает зависшую заявку в pending и обрабатывает её.
	startWorker(t, db, provider, cfg)

	u := requireDone(t, svc, id)
	if got := u.Price.Or(""); got != "17.2" {
		t.Errorf("price = %q, want 17.2", got)
	}
}

// setup подключается к БД из config.json и создаёт Service.
func setup(t *testing.T) (*implementations.Service, *database.DB, config.Config) {
	t.Helper()

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	db, err := database.New(context.Background(), cfg.Database.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	return implementations.NewService(db, db, cfg.HTTP.APIKey, logger(t)), db, cfg
}

func workerConfig() implementations.WorkerConfig {
	return implementations.WorkerConfig{
		PollInterval: 50 * time.Millisecond,
		BatchSize:    100,
		MaxAttempts:  3,
		LockTimeout:  time.Minute,
	}
}

// startWorker запускает воркер и останавливает его после теста.
func startWorker(t *testing.T, db *database.DB, provider implementations.RateProvider, cfg implementations.WorkerConfig) {
	t.Helper()

	worker := implementations.NewWorker(db, provider, cfg, logger(t))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { worker.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func logger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}

func createUpdate(t *testing.T, svc *implementations.Service, key uuid.UUID, base, quote api.Currency) uuid.UUID {
	t.Helper()

	resp, err := svc.CreateQuoteUpdate(context.Background(),
		&api.CreateUpdateRequest{Base: base, Quote: quote},
		api.CreateQuoteUpdateParams{IdempotencyKey: key})
	if err != nil {
		t.Fatalf("CreateQuoteUpdate %s/%s: %v", base, quote, err)
	}
	return resp.UpdateID
}

// waitFinished ждёт, пока заявка перейдёт в done или failed.
func waitFinished(t *testing.T, svc *implementations.Service, id uuid.UUID) *api.QuoteUpdate {
	t.Helper()

	var u *api.QuoteUpdate
	waitFor(t, "update "+id.String()+" finished", func() bool {
		var err error
		u, err = svc.GetQuoteUpdate(context.Background(), api.GetQuoteUpdateParams{ID: id})
		if err != nil {
			t.Fatalf("GetQuoteUpdate %s: %v", id, err)
		}
		return u.Status == api.UpdateStatusDone || u.Status == api.UpdateStatusFailed
	})
	return u
}

// requireDone ждёт завершения заявки и проверяет, что она done.
func requireDone(t *testing.T, svc *implementations.Service, id uuid.UUID) *api.QuoteUpdate {
	t.Helper()

	u := waitFinished(t, svc, id)
	if u.Status != api.UpdateStatusDone {
		t.Fatalf("update %s: status %s, want done (error %q)", id, u.Status, u.Error.Or(""))
	}
	return u
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireStatus проверяет, что ошибка есть и Service.NewError отдаёт её с кодом want.
func requireStatus(t *testing.T, svc *implementations.Service, err error, want int) {
	t.Helper()

	if err == nil {
		t.Fatalf("got no error, want HTTP %d", want)
	}
	if got := svc.NewError(context.Background(), err).StatusCode; got != want {
		t.Fatalf("error %q maps to HTTP %d, want %d", err, got, want)
	}
}

// now — текущее время с точностью до секунды: курсы тестов новее уже
// сохранённых в БД.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

// fakeProvider отдаёт курс price со временем updatedAt для любой пары
// или ошибку err.
type fakeProvider struct {
	mu        sync.Mutex
	price     string
	updatedAt time.Time
	err       error
	calls     map[string]int // base → число вызовов
	total     int

	// block, если задан, держит первый вызов GetRates, пока канал не закроют.
	block chan struct{}
}

func (p *fakeProvider) GetRates(_ context.Context, base string, quotes ...string) (map[string]exchangerates.Rate, error) {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = make(map[string]int)
	}
	p.calls[base]++
	p.total++
	first := p.total == 1
	price, updatedAt, err := p.price, p.updatedAt, p.err
	p.mu.Unlock()

	if first && p.block != nil {
		<-p.block
	}
	if err != nil {
		return nil, err
	}

	rates := make(map[string]exchangerates.Rate, len(quotes))
	for _, quote := range quotes {
		rates[quote] = exchangerates.Rate{
			Base:      base,
			Quote:     quote,
			Price:     decimal.RequireFromString(price),
			UpdatedAt: updatedAt,
		}
	}
	return rates, nil
}

func (p *fakeProvider) set(price string, updatedAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.price, p.updatedAt = price, updatedAt
}

func (p *fakeProvider) callsFor(base string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[base]
}
