# API

Base URL: `http://localhost:8080`. All bodies are JSON. Errors are returned as `{"error": "..."}`.

## Running

```bash
cp .env.example .env    # set jwt_secret
docker compose up -d --build
curl localhost:8080/health
```

Tables are created automatically on startup.

| Env var              | Default | Meaning                                       |
|----------------------|---------|-----------------------------------------------|
| `DATABASE_URL`       | local   | Postgres connection string                    |
| `APP_PORT`           | `8080`  | HTTP port                                     |
| `jwt_secret`         | —       | HMAC secret for access tokens (required)      |
| `salt_rounds`        | `10`    | bcrypt cost                                   |
| `validation_timeout` | `15m`   | how long a held reservation blocks its seats  |

---

## Health

| Endpoint | Checks | Use it for |
|----------|--------|------------|
| `GET /health/live` | Only that the process is serving HTTP; never touches the database | Liveness: restart the app if this fails |
| `GET /health/ready` | Runs `SELECT 1` on Postgres within 2 seconds | Readiness: send traffic only while this is `200` |
| `GET /health` | Same as `/health/ready` | Kept for compatibility |

Liveness always returns `200 {"status": "ok"}`, so a database outage doesn't
get the app restarted for nothing.

Readiness fails closed. Any database error, or no answer within 2 seconds,
returns `503 {"status": "unavailable", "postgres": "unreachable"}`. When it's
healthy it returns `200 {"status": "ok", "postgres": "ok"}`. The underlying
error is logged by the server, not returned to the caller.

## Metrics — `GET /metrics`

Prometheus text format. The full list of metrics and how they reconcile with
the API is in the [README](../README.md#metrics).

```bash
curl -s localhost:8080/metrics | grep ^booking_
```

```
booking_reservations_confirmed_total 805
booking_reservations_declined_total{reason="seat_taken"} 16695
booking_reservations_declined_total{reason="per_user_limit"} 300
booking_reservations_declined_total{reason="idempotent_replay"} 1800
booking_seats_available{show_id="520e252d-..."} 1095
booking_seats_held{show_id="520e252d-..."} 0
booking_seats_confirmed{show_id="520e252d-..."} 1405
booking_seats_capacity{show_id="520e252d-..."} 2500
```

For each show, the four `booking_seats_*` gauges equal the `counts` returned
by `GET /shows/{id}`.

## Register — `POST /user/register`

```bash
curl -X POST localhost:8080/user/register \
  -d '{"user_id": "alice", "password": "secret123"}'
```

| Status | Meaning |
|--------|---------|
| 200 | `{"user_id": "alice"}` |
| 400 | missing `user_id`/`password`, or password over 72 bytes |
| 409 | `user_id` already exists |

## Login — `POST /user/login`

```bash
curl -X POST localhost:8080/user/login \
  -d '{"user_id": "alice", "password": "secret123"}'
```

```json
{
  "user_id": "alice",
  "jwt_token": "eyJhbGciOi...",
  "expires_at": "2026-10-04T09:15:00Z",
  "refresh_token": "9f2c..."
}
```

The `jwt_token` is valid for 15 minutes. Send it as `Authorization: Bearer <jwt_token>`.
The refresh token is stored in `user_sessions` (valid 7 days); there is no refresh endpoint yet.

| Status | Meaning |
|--------|---------|
| 200 | logged in |
| 401 | incorrect password |
| 404 | user not found |

## Create a show — `POST /shows`

Unauthenticated (admin).

```bash
curl -X POST localhost:8080/shows \
  -d '{"name": "friday-night", "seats": ["A1","A2","A3","B1","B2"], "price_paise": 25000}'
```

| Field | Required | Notes |
|-------|----------|-------|
| `name` | yes | |
| `seats` | yes | unique names; trimmed and upper-cased |
| `price_paise` | yes | price per seat, `>= 0` |
| `limit_per_user` | no | max seats one user can book for the show, default 4 |

`201` returns the show in the same shape as `GET /shows/{id}`, with every seat `available`.

## Show state — `GET /shows/{id}`

```bash
curl localhost:8080/shows/<show_id>
```

```json
{
  "show_id": "61f8dbdb-d40f-48ce-82fa-ca6bfc3fb9db",
  "name": "friday-night",
  "price_paise": 25000,
  "limit_per_user": 4,
  "counts": { "total_seats": 5, "available": 3, "held": 0, "confirmed": 2 },
  "seats": [
    { "name": "A1", "status": "confirmed" },
    { "name": "A2", "status": "confirmed" },
    { "name": "A3", "status": "available" }
  ]
}
```

`available + held + confirmed == total_seats` always holds. A hold past its
`valid_upto` is reported as `available`.

`404` if the show does not exist.

## Reserve seats — `POST /shows/{id}/reserve`

Authenticated. The user comes from the token, never the body. The idempotency
key can go in the body or in an `Idempotency-Key` header (if both are sent they must match).

```bash
curl -X POST localhost:8080/shows/<show_id>/reserve \
  -H "Authorization: Bearer <jwt_token>" \
  -d '{"seats": ["A1","A2"], "idempotency_key": "6c1d3c1e-..."}'
```

`201`:

```json
{
  "reservation_id": "6c1d3c1e-...",
  "show_id": "61f8dbdb-...",
  "user_id": "alice",
  "seats": ["A1", "A2"],
  "amount_paise": 50000,
  "status": "confirmed"
}
```

Seats are claimed as `held` and then immediately `confirmed`.

**Retries:** repeating a request with the same key, user, show and seats (in
any order) returns the original reservation with `201` and an
`Idempotent-Replayed: true` header. Reusing the key for anything else is a `409`.

**Partly available:** if any requested seat is taken, nothing is booked and the
response suggests the same number of seats, as close together as possible:

```json
{
  "error": "some requested seats are not available",
  "unavailable_seats": ["A2"],
  "suggested_seats": ["B1", "B2"]
}
```

| Status | Meaning |
|--------|---------|
| 201 | booked (or replayed) |
| 400 | bad body, missing key, or `unknown_seats` listed |
| 401 | missing or invalid token |
| 404 | show not found |
| 409 | seats taken (with suggestion), seat limit exceeded, key reused, or lost a race |

A race for the same seat produces exactly one `201`; every loser gets a `409`.

The user always comes from the token: unknown body fields, such as a spoofed
`user_id`, are ignored.

## Cancel a reservation — `DELETE /reservations/{id}`

Authenticated. `{id}` is the `reservation_id` (the idempotency key used to book).
Only the user who made the reservation can cancel it.

```bash
curl -X DELETE localhost:8080/reservations/6c1d3c1e-... \
  -H "Authorization: Bearer <jwt_token>"
```

`200` returns the reservation with `"status": "cancelled"`. Its seats become
`available` again and stop counting toward the user's `limit_per_user`. If a
hold had already expired and another user took one of its seats, that seat
stays with them.

Cancelling an already cancelled reservation returns `200` again. Retrying the
original reserve request with the same key returns the cancelled reservation
and books nothing; use a new key to book again.

| Status | Meaning |
|--------|---------|
| 200 | cancelled (or already was) |
| 401 | missing or invalid token |
| 404 | no such reservation, or it belongs to another user |
| 409 | the reservation is being changed by another request, retry |
