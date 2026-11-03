package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// ClaimedQuoteUpdate — заявка, которую воркер забрал в обработку.
type ClaimedQuoteUpdate struct {
	ID    uuid.UUID
	Base  string
	Quote string
}

// RateResult — полученный курс и заявки, которые им закрываются.
type RateResult struct {
	Base      string
	Quote     string
	Price     decimal.Decimal
	UpdatedAt time.Time // время курса у провайдера
	UpdateIDs []uuid.UUID
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

// CompleteQuoteUpdates в одной транзакции сохраняет полученные от провайдера
// курсы и переводит связанные с ними заявки в done. Если курс уже есть,
// у него обновляется fetched_at: провайдер только что его подтвердил.
func (db *DB) CompleteQuoteUpdates(ctx context.Context, results []RateResult) error {
	const insertRate = `
		INSERT INTO rates (base, quote, price, rate_updated_at)
		VALUES ($1, $2, $3::numeric, $4)
		ON CONFLICT (base, quote, rate_updated_at)
		DO UPDATE SET price = EXCLUDED.price, fetched_at = now()
		RETURNING id`

	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		for _, r := range results {
			var rateID int64
			err := tx.QueryRow(ctx, insertRate, r.Base, r.Quote, r.Price.String(), r.UpdatedAt).Scan(&rateID)
			if err != nil {
				return fmt.Errorf("complete quote updates: insert rate %s/%s: %w", r.Base, r.Quote, err)
			}

			if _, err := tx.Exec(ctx, completeUpdatesQuery, rateID, uuidStrings(r.UpdateIDs)); err != nil {
				return fmt.Errorf("complete quote updates: %s/%s: %w", r.Base, r.Quote, err)
			}
		}
		return nil
	})
}

// GetFreshRateIDs возвращает id свежих курсов base к quotes: тех, что провайдер
// вернул не раньше ttl назад (по fetched_at). Если таких курсов по паре
// несколько, берётся самый новый по времени курса у провайдера. Quote без
// свежего курса в ответе нет.
func (db *DB) GetFreshRateIDs(ctx context.Context, base string, quotes []string, ttl time.Duration) (map[string]int64, error) {
	const q = `
		SELECT DISTINCT ON (quote) quote, id
		FROM rates
		WHERE base = $1 AND quote = ANY($2::bpchar[])
		  AND fetched_at > now() - make_interval(secs => $3)
		ORDER BY quote, rate_updated_at DESC`

	rows, err := db.pool.Query(ctx, q, base, quotes, ttl.Seconds())
	if err != nil {
		return nil, fmt.Errorf("get fresh rates: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]int64)
	for rows.Next() {
		var (
			quote string
			id    int64
		)
		if err := rows.Scan(&quote, &id); err != nil {
			return nil, fmt.Errorf("get fresh rates: %w", err)
		}
		ids[quote] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get fresh rates: %w", err)
	}

	return ids, nil
}

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

func uuidStrings(ids []uuid.UUID) []string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return s
}
