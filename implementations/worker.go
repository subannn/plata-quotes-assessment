package implementations

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"plata-quotes-assessment/internal/database"
	"plata-quotes-assessment/internal/exchangerates"
)

var (
	_ RateProvider = (*exchangerates.Client)(nil)
	_ WorkerStore  = (*database.DB)(nil)
)

// RateProvider — источник курсов валют.
type RateProvider interface {
	GetRates(ctx context.Context, base string, quotes ...string) (map[string]exchangerates.Rate, error)
}

// WorkerStore — операции с очередью заявок, которые нужны воркеру.
type WorkerStore interface {
	ClaimPendingQuoteUpdates(ctx context.Context, limit int) ([]database.ClaimedQuoteUpdate, error)
	CompleteQuoteUpdates(ctx context.Context, results []database.RateResult) error
	GetFreshRateIDs(ctx context.Context, base string, quotes []string, ttl time.Duration) (map[string]int64, error)
	CompleteQuoteUpdatesWithRate(ctx context.Context, rateID int64, ids []uuid.UUID) error
	FailQuoteUpdates(ctx context.Context, ids []uuid.UUID, reason string, maxAttempts int) error
	ReleaseStuckQuoteUpdates(ctx context.Context, lockTimeout time.Duration, maxAttempts int) (int64, error)
}

type WorkerConfig struct {
	PollInterval time.Duration // как часто проверять новые заявки
	BatchSize    int           // сколько заявок забирать за раз
	MaxAttempts  int           // после стольких неудачных попыток заявка — failed
	LockTimeout  time.Duration // через сколько processing считается зависшей
	RateTTL      time.Duration // сколько курс от провайдера считается свежим; 0 — без кеша
}

// Worker периодически забирает заявки pending из БД, получает курсы
// у провайдера и сохраняет результат.
type Worker struct {
	store    WorkerStore
	provider RateProvider
	cfg      WorkerConfig
	log      *slog.Logger
}

func NewWorker(store WorkerStore, provider RateProvider, cfg WorkerConfig, log *slog.Logger) *Worker {
	return &Worker{store: store, provider: provider, cfg: cfg, log: log}
}

// Run обрабатывает заявки раз в PollInterval, пока не отменён ctx.
// Начатый цикл доводится до конца даже после отмены ctx.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		w.tick(context.WithoutCancel(ctx))

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	released, err := w.store.ReleaseStuckQuoteUpdates(ctx, w.cfg.LockTimeout, w.cfg.MaxAttempts)
	if err != nil {
		w.log.Error("release stuck quote updates", "error", err)
	} else if released > 0 {
		w.log.Warn("released stuck quote updates", "count", released)
	}

	claimed, err := w.store.ClaimPendingQuoteUpdates(ctx, w.cfg.BatchSize)
	if err != nil {
		w.log.Error("claim quote updates", "error", err)
		return
	}

	for base, byQuote := range groupByBase(claimed) {
		w.processGroup(ctx, base, byQuote)
	}
}

// processGroup закрывает заявки группы с одной base. Пары, по которым
// есть свежий курс, закрываются им без запроса к провайдеру; курсы остальных
// берутся у провайдера одним запросом.
func (w *Worker) processGroup(ctx context.Context, base string, byQuote map[string][]uuid.UUID) {
	quotes := w.completeFromCache(ctx, base, byQuote)
	if len(quotes) == 0 {
		return
	}

	var ids []uuid.UUID
	for _, quote := range quotes {
		ids = append(ids, byQuote[quote]...)
	}
	log := w.log.With("base", base, "quotes", quotes, "updates", len(ids))

	rates, err := w.provider.GetRates(ctx, base, quotes...)
	if err != nil {
		log.Warn("get rates", "error", err)
		w.fail(ctx, log, ids, err)
		return
	}

	results := make([]database.RateResult, 0, len(quotes))
	for _, quote := range quotes {
		rate := rates[quote]
		results = append(results, database.RateResult{
			Base:      rate.Base,
			Quote:     rate.Quote,
			Price:     rate.Price,
			UpdatedAt: rate.UpdatedAt,
			UpdateIDs: byQuote[quote],
		})
	}

	if err := w.store.CompleteQuoteUpdates(ctx, results); err != nil {
		log.Error("complete quote updates", "error", err)
		w.fail(ctx, log, ids, err)
		return
	}

	log.Info("quote updates done")
}

// completeFromCache закрывает заявки по парам со свежим курсом в БД и
// возвращает отсортированные quote, за которыми нужно идти к провайдеру.
func (w *Worker) completeFromCache(ctx context.Context, base string, byQuote map[string][]uuid.UUID) []string {
	quotes := slices.Sorted(maps.Keys(byQuote))
	if w.cfg.RateTTL <= 0 {
		return quotes
	}

	fresh, err := w.store.GetFreshRateIDs(ctx, base, quotes, w.cfg.RateTTL)
	if err != nil {
		// Кеш — оптимизация: без него идём к провайдеру по всем парам.
		w.log.Warn("get fresh rates", "base", base, "error", err)
		return quotes
	}

	var toFetch []string
	for _, quote := range quotes {
		rateID, ok := fresh[quote]
		if !ok {
			toFetch = append(toFetch, quote)
			continue
		}

		ids := byQuote[quote]
		log := w.log.With("base", base, "quote", quote, "updates", len(ids), "rate_id", rateID)
		if err := w.store.CompleteQuoteUpdatesWithRate(ctx, rateID, ids); err != nil {
			log.Error("complete quote updates from cache", "error", err)
			w.fail(ctx, log, ids, err)
			continue
		}
		log.Info("quote updates done from cache")
	}

	return toFetch
}

func (w *Worker) fail(ctx context.Context, log *slog.Logger, ids []uuid.UUID, reason error) {
	if err := w.store.FailQuoteUpdates(ctx, ids, reason.Error(), w.cfg.MaxAttempts); err != nil {
		// Заявки остались в processing — их вернёт ReleaseStuckQuoteUpdates.
		log.Error("fail quote updates", "error", err)
	}
}

// groupByBase раскладывает заявки по base, а внутри — по quote.
func groupByBase(updates []database.ClaimedQuoteUpdate) map[string]map[string][]uuid.UUID {
	groups := make(map[string]map[string][]uuid.UUID)
	for _, u := range updates {
		if groups[u.Base] == nil {
			groups[u.Base] = make(map[string][]uuid.UUID)
		}
		groups[u.Base][u.Quote] = append(groups[u.Base][u.Quote], u.ID)
	}
	return groups
}
