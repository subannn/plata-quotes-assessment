package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// QuoteUpdate — заявка на обновление котировки вместе с полученным курсом.
type QuoteUpdate struct {
	ID     uuid.UUID
	Base   string
	Quote  string
	Status string

	// Price и RateUpdatedAt заданы, когда к заявке привязан курс (status = done).
	Price         *decimal.Decimal
	RateUpdatedAt *time.Time

	// Error задан при status = failed.
	Error *string
}

// ClaimedQuoteUpdate — заявка, которую воркер забрал в обработку.
type ClaimedQuoteUpdate struct {
	ID    uuid.UUID
	Base  string
	Quote string
}

// InsertQuoteUpdate создаёт заявку на обновление котировки base/quote в статусе
// pending и возвращает её id.
//
// Если заявка с таким idempotencyKey уже есть, новая не создаётся:
// для той же пары возвращается id существующей заявки, для другой пары —
// ErrIdempotencyConflict.
func (db *DB) InsertQuoteUpdate(ctx context.Context, base, quote string, idempotencyKey uuid.UUID) (uuid.UUID, error) {
	const insert = `
		INSERT INTO quote_updates (base, quote, idempotency_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`

	// Отдельным запросом, а не в том же INSERT: при одновременной вставке
	// с тем же ключом INSERT дожидается коммита чужой заявки, и только новый
	// запрос (с новым снимком данных) её увидит.
	const selectExisting = `
		SELECT id, base, quote
		FROM quote_updates
		WHERE idempotency_key = $1`

	var id uuid.UUID
	err := db.pool.QueryRow(ctx, insert, base, quote, idempotencyKey).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("insert quote update: %w", err)
	}

	// Конфликт по ключу — заявка уже создана.
	var existingBase, existingQuote string
	err = db.pool.QueryRow(ctx, selectExisting, idempotencyKey).Scan(&id, &existingBase, &existingQuote)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert quote update: get existing by idempotency key: %w", err)
	}

	if existingBase != base || existingQuote != quote {
		return uuid.Nil, fmt.Errorf("%w: key %s is used for %s/%s", ErrIdempotencyConflict, idempotencyKey, existingBase, existingQuote)
	}

	return id, nil
}

// GetQuoteUpdate возвращает заявку по id. Если заявки нет — ErrNotFound.
func (db *DB) GetQuoteUpdate(ctx context.Context, id uuid.UUID) (QuoteUpdate, error) {
	const q = `
		SELECT u.id, u.base, u.quote, u.status, u.error, r.price::text, r.rate_updated_at
		FROM quote_updates u
		LEFT JOIN rates r ON r.id = u.rate_id
		WHERE u.id = $1`

	var (
		u     QuoteUpdate
		price *string
	)
	err := db.pool.QueryRow(ctx, q, id).Scan(&u.ID, &u.Base, &u.Quote, &u.Status, &u.Error, &price, &u.RateUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuoteUpdate{}, ErrNotFound
	}
	if err != nil {
		return QuoteUpdate{}, fmt.Errorf("get quote update: %w", err)
	}

	if price != nil {
		d, err := decimal.NewFromString(*price)
		if err != nil {
			return QuoteUpdate{}, fmt.Errorf("get quote update: parse price %q: %w", *price, err)
		}
		u.Price = &d
	}

	return u, nil
}

// ClaimPendingQuoteUpdates берёт в работу до limit заявок в статусе pending,
// начиная с самых старых: переводит их в processing, ставит locked_at = now()
// и увеличивает attempts. Возвращает взятые заявки.
//
// Выборка и смена статуса — один запрос, поэтому одну заявку не могут взять
// два экземпляра сервиса: строки, которые в этот момент забирает другой
// экземпляр, пропускаются (FOR UPDATE SKIP LOCKED), а не ждут его.
func (db *DB) ClaimPendingQuoteUpdates(ctx context.Context, limit int) ([]ClaimedQuoteUpdate, error) {
	const q = `
		UPDATE quote_updates
		SET status = 'processing', locked_at = now(), attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM quote_updates
			WHERE status = 'pending'
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, base, quote`

	rows, err := db.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("claim quote updates: %w", err)
	}

	claimed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClaimedQuoteUpdate, error) {
		var u ClaimedQuoteUpdate
		err := row.Scan(&u.ID, &u.Base, &u.Quote)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim quote updates: %w", err)
	}

	return claimed, nil
}

// completeUpdatesQuery переводит заявки $2 в done с курсом $1.
const completeUpdatesQuery = `
	UPDATE quote_updates
	SET status = 'done', rate_id = $1, error = NULL, locked_at = NULL, updated_at = now()
	WHERE id = ANY($2::uuid[]) AND status = 'processing'`

// CompleteQuoteUpdatesWithRate переводит заявки в done с уже сохранённым
// курсом rateID. Сам курс не трогает: fetched_at обновляет только ответ
// провайдера, иначе кеш никогда бы не устаревал.
func (db *DB) CompleteQuoteUpdatesWithRate(ctx context.Context, rateID int64, ids []uuid.UUID) error {
	if _, err := db.pool.Exec(ctx, completeUpdatesQuery, rateID, uuidStrings(ids)); err != nil {
		return fmt.Errorf("complete quote updates with rate %d: %w", rateID, err)
	}

	return nil
}

// FailQuoteUpdates обрабатывает неудачную попытку: заявки, у которых попытки
// закончились (attempts >= maxAttempts), переводятся в failed с текстом ошибки,
// остальные возвращаются в pending для повтора.
func (db *DB) FailQuoteUpdates(ctx context.Context, ids []uuid.UUID, reason string, maxAttempts int) error {
	const q = `
		UPDATE quote_updates
		SET status    = CASE WHEN attempts >= $3 THEN 'failed' ELSE 'pending' END,
		    error     = CASE WHEN attempts >= $3 THEN $2 END,
		    locked_at = NULL,
		    updated_at = now()
		WHERE id = ANY($1::uuid[]) AND status = 'processing'`

	if _, err := db.pool.Exec(ctx, q, uuidStrings(ids), reason, maxAttempts); err != nil {
		return fmt.Errorf("fail quote updates: %w", err)
	}

	return nil
}

// ReleaseStuckQuoteUpdates возвращает в pending заявки, которые находятся
// в processing дольше lockTimeout (воркер упал или завис). Если попытки
// закончились — заявка переводится в failed. Возвращает число затронутых заявок.
func (db *DB) ReleaseStuckQuoteUpdates(ctx context.Context, lockTimeout time.Duration, maxAttempts int) (int64, error) {
	const q = `
		UPDATE quote_updates
		SET status    = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
		    error     = CASE WHEN attempts >= $2 THEN 'processing timed out' END,
		    locked_at = NULL,
		    updated_at = now()
		WHERE status = 'processing' AND locked_at < now() - make_interval(secs => $1)`

	tag, err := db.pool.Exec(ctx, q, lockTimeout.Seconds(), maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("release stuck quote updates: %w", err)
	}

	return tag.RowsAffected(), nil
}
