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

// Rate — курс пары от провайдера.
type Rate struct {
	Base      string
	Quote     string
	Price     decimal.Decimal
	UpdatedAt time.Time // время курса у провайдера
}

// RateResult — полученный курс и заявки, которые им закрываются.
type RateResult struct {
	Base      string
	Quote     string
	Price     decimal.Decimal
	UpdatedAt time.Time // время курса у провайдера
	UpdateIDs []uuid.UUID
}

// GetLatestRate возвращает самый свежий курс пары base/quote по времени курса
// у провайдера. Если at задан — самый свежий курс на момент at или раньше.
// Если курса нет — ErrNotFound.
func (db *DB) GetLatestRate(ctx context.Context, base, quote string, at *time.Time) (Rate, error) {
	const q = `
		SELECT base, quote, price::text, rate_updated_at
		FROM rates
		WHERE base = $1 AND quote = $2
		  AND ($3::timestamptz IS NULL OR rate_updated_at <= $3)
		ORDER BY rate_updated_at DESC
		LIMIT 1`

	var (
		r     Rate
		price string
	)
	err := db.pool.QueryRow(ctx, q, base, quote, at).Scan(&r.Base, &r.Quote, &price, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, ErrNotFound
	}
	if err != nil {
		return Rate{}, fmt.Errorf("get latest rate: %w", err)
	}

	r.Price, err = decimal.NewFromString(price)
	if err != nil {
		return Rate{}, fmt.Errorf("get latest rate: parse price %q: %w", price, err)
	}

	return r, nil
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
