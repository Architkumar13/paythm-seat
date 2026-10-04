CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS shows (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    per_user_limit INTEGER NOT NULL CHECK (per_user_limit >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per physical seat. The primary key makes a second copy of A12 impossible.
-- status is the system of record: available, held, or confirmed.
CREATE TABLE IF NOT EXISTS seats (
    show_id TEXT NOT NULL REFERENCES shows (id),
    seat_label TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('available', 'held', 'confirmed')),
    reservation_id TEXT,
    user_id TEXT,
    PRIMARY KEY (show_id, seat_label)
);

CREATE INDEX IF NOT EXISTS seats_show_status_idx ON seats (show_id, status);

CREATE TABLE IF NOT EXISTS reservations (
    id TEXT PRIMARY KEY,
    show_id TEXT NOT NULL REFERENCES shows (id),
    user_id TEXT NOT NULL REFERENCES users (id),
    seats TEXT[] NOT NULL,
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    status TEXT NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancelled_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS reservations_user_show_idx ON reservations (user_id, show_id);

-- Serialized per (user, show). The limit check updates this row, it does not read-then-write in the app.
CREATE TABLE IF NOT EXISTS user_show_counts (
    user_id TEXT NOT NULL REFERENCES users (id),
    show_id TEXT NOT NULL REFERENCES shows (id),
    confirmed_seats INTEGER NOT NULL CHECK (confirmed_seats >= 0),
    PRIMARY KEY (user_id, show_id)
);

-- Exactly-once scope is (user_id, idempotency_key). request_hash binds the body.
-- A decline is stored too, so a retry replays the first outcome instead of taking a seat later.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id TEXT NOT NULL REFERENCES users (id),
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    show_id TEXT NOT NULL,
    reservation_id TEXT REFERENCES reservations (id),
    result_code TEXT,
    result_seats TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, idempotency_key)
);
