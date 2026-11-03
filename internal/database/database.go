package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound — запись не найдена.
	ErrNotFound = errors.New("not found")

	// ErrIdempotencyConflict — Idempotency-Key уже использован для заявки
	// с другой парой.
	ErrIdempotencyConflict = errors.New("idempotency key already used for another currency pair")
)

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

func uuidStrings(ids []uuid.UUID) []string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return s
}
