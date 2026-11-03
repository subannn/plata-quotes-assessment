-- Котировки, полученные от провайдера.
CREATE TABLE IF NOT EXISTS rates (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    base             CHAR(3)         NOT NULL CHECK (base  ~ '^[A-Z]{3}$'),
    quote            CHAR(3)         NOT NULL CHECK (quote ~ '^[A-Z]{3}$'),
    price            NUMERIC(24, 10) NOT NULL CHECK (price > 0),
    rate_updated_at  TIMESTAMPTZ     NOT NULL,
    fetched_at       TIMESTAMPTZ     NOT NULL DEFAULT now(),

    CONSTRAINT rates_base_differs_from_quote CHECK (base <> quote),
    CONSTRAINT rates_pair_time_uniq UNIQUE (base, quote, rate_updated_at)
);

-- Заявки на обновление котировок.
CREATE TABLE IF NOT EXISTS quote_updates (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    base             CHAR(3)     NOT NULL CHECK (base  ~ '^[A-Z]{3}$'),
    quote            CHAR(3)     NOT NULL CHECK (quote ~ '^[A-Z]{3}$'),
    status           TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    rate_id          BIGINT      REFERENCES rates (id),
    error            TEXT,
    attempts         INT         NOT NULL DEFAULT 0,
    idempotency_key  UUID        NOT NULL,
    locked_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT quote_updates_base_differs_from_quote CHECK (base <> quote),
    CONSTRAINT quote_updates_done_has_rate CHECK (status <> 'done' OR rate_id IS NOT NULL),
    CONSTRAINT quote_updates_idempotency_key_uniq UNIQUE (idempotency_key)
);

-- Воркер: выборка ожидающих заявок по очереди.
CREATE INDEX IF NOT EXISTS quote_updates_pending_idx
    ON quote_updates (created_at)
    WHERE status = 'pending';

-- Воркер: поиск зависших заявок.
CREATE INDEX IF NOT EXISTS quote_updates_processing_idx
    ON quote_updates (locked_at)
    WHERE status = 'processing';