CREATE TABLE IF NOT EXISTS users (
    user_id         TEXT PRIMARY KEY,
    hashed_password TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_sessions (
    session_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       TEXT NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    refresh_token TEXT NOT NULL UNIQUE,
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS shows (
    show_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    limit_per_user INT NOT NULL DEFAULT 4 CHECK (limit_per_user > 0),
    price_paise    BIGINT NOT NULL CHECK (price_paise >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS seats (
    seat_id BIGSERIAL PRIMARY KEY,
    name    TEXT NOT NULL,
    show_id UUID NOT NULL REFERENCES shows (show_id) ON DELETE CASCADE,
    UNIQUE (show_id, name)
);

-- reservation_id is the client's idempotency key.
CREATE TABLE IF NOT EXISTS reservations (
    reservation_id TEXT PRIMARY KEY,
    seats          JSONB NOT NULL,
    user_id        TEXT NOT NULL REFERENCES users (user_id),
    show_id        UUID NOT NULL REFERENCES shows (show_id),
    status         TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'held', 'confirmed')),
    amount_paise   BIGINT NOT NULL,
    valid_upto     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reservations_user_show_idx ON reservations (user_id, show_id);

-- Reservations can be cancelled; widen the status check on databases created before that.
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_status_check;
ALTER TABLE reservations ADD CONSTRAINT reservations_status_check
    CHECK (status IN ('available', 'held', 'confirmed', 'cancelled'));

-- One row per seat of a show; this is the row that gets locked when a seat is claimed.
-- A 'held' seat whose valid_upto has passed counts as available (lazy expiry).
CREATE TABLE IF NOT EXISTS show_seats (
    show_id        UUID NOT NULL REFERENCES shows (show_id) ON DELETE CASCADE,
    seat_id        BIGINT NOT NULL REFERENCES seats (seat_id) ON DELETE CASCADE,
    status         TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'held', 'confirmed')),
    reservation_id TEXT REFERENCES reservations (reservation_id),
    valid_upto     TIMESTAMPTZ,
    PRIMARY KEY (show_id, seat_id)
);

CREATE INDEX IF NOT EXISTS show_seats_reservation_idx ON show_seats (reservation_id);
