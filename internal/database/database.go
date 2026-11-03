package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

var (
	// ErrNotFound — запись не найдена.
	ErrNotFound = errors.New("not found")

	// ErrIdempotencyConflict — Idempotency-Key уже использован для заявки
	// с другой парой.
	ErrIdempotencyConflict = errors.New("idempotency key already used for another currency pair")
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

// DB — доступ к PostgreSQL.
type DB struct {
	pool *pgxpool.Pool
}

// New подключается к PostgreSQL по url и проверяет соединение. Схема БД
// (migration/init.up.sql) должна быть применена заранее. После работы
// соединения закрываются через Close.
func New(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Close закрывает соединения с БД.
func (db *DB) Close() {
	db.pool.Close()
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

// Rate — курс пары от провайдера.
type Rate struct {
	Base      string
	Quote     string
	Price     decimal.Decimal
	UpdatedAt time.Time // время курса у провайдера
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
