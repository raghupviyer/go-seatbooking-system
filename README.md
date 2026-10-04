# Go Ticket Booking System

A movie ticket booking API in Go and Postgres. Users register, log in, and
reserve seats for a show. It's built so a seat can never be sold twice, even
when thousands of requests race for the same seat.

**Stack:** Go 1.27, PostgreSQL 16, Docker

## Features

- **Auth:** register and log in with a bcrypt-hashed password. Login returns a
  15-minute JWT, and every reservation uses the identity in that token.
- **Shows:** create a show with its seats, price and per-user seat limit.
- **Reservations:** reserve one or more seats with an idempotency key. Seats are
  held, then confirmed.
- **Seat suggestions:** if some requested seats are taken, the response suggests
  the same number of free seats, kept as close together as possible.
- **Cancellation:** a user can cancel only their own reservation, which frees
  its seats.
- **Health checks:** separate liveness and readiness endpoints.

## Running locally

```bash
cp .env.example .env    # then set jwt_secret
docker compose up -d --build
curl localhost:8080/health/ready
```

The app creates its tables on startup from
[internal/store/schema.sql](internal/store/schema.sql), so there's no migration
step to run.

## Environment variables

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DATABASE_URL` | Yes | local Postgres | Postgres connection string |
| `jwt_secret` | **Yes** | none | Secret for signing JWTs; the app won't start without it |
| `salt_rounds` | No | `10` | bcrypt cost |
| `validation_timeout` | No | `15m` | How long a held reservation blocks its seats |
| `APP_PORT` | No | `8080` | HTTP port |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn` or `error`. Health and metrics requests log at `debug` |

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health/live` | No | Liveness |
| `GET` | `/health/ready` | No | Readiness (checks the database) |
| `GET` | `/metrics` | No | Prometheus metrics |
| `POST` | `/user/register` | No | Create a user |
| `POST` | `/user/login` | No | Get a JWT |
| `POST` | `/shows` | No | Create a show |
| `GET` | `/shows/{id}` | No | Seat-by-seat state and counts |
| `POST` | `/shows/{id}/reserve` | Yes | Reserve seats |
| `DELETE` | `/reservations/{id}` | Yes | Cancel your own reservation |

Request and response examples for every endpoint are in
[docs/API.md](docs/API.md).

## Health checks

- **`GET /health/live`** only confirms the process is serving HTTP. It never
  touches the database, so a database outage doesn't get the app restarted.
- **`GET /health/ready`** runs `SELECT 1` on Postgres with a 2-second limit.
  It fails closed: any error, or no answer in time, returns `503`.
- **`GET /health`** is an alias for readiness.

| Database | `/health/live` | `/health/ready` |
|---|---|---|
| Up | `200` | `200` |
| Stopped | `200` | `503` |
| Frozen (connections hang) | `200` | `503` after about 2 seconds |
| Back up | `200` | `200`, reconnects by itself |

The readiness response says only `"postgres": "unreachable"`. The actual error
goes to the server log, so internal details aren't exposed.

## Metrics

`GET /metrics` serves Prometheus metrics, so you can watch a burst of
reservations as it happens.

| Metric | Type | Meaning |
|---|---|---|
| `booking_reservations_confirmed_total` | Counter | New bookings, counted after the database commit |
| `booking_reservations_declined_total{reason}` | Counter | Reserve requests that created no new booking, by reason |
| `booking_reservations_cancelled_total` | Counter | Reservations cancelled (a repeat cancel isn't counted) |
| `booking_seats_available{show_id}` | Gauge | Seats that can be reserved now |
| `booking_seats_held{show_id}` | Gauge | Seats under an unexpired hold |
| `booking_seats_confirmed{show_id}` | Gauge | Seats sold |
| `booking_seats_capacity{show_id}` | Gauge | Total seats in the show |
| `booking_http_requests_total{route,code}` | Counter | Requests by route and status code |
| `booking_http_request_duration_seconds{route}` | Histogram | Request latency by route |

The `reason` label is one of `seat_taken`, `per_user_limit`,
`idempotent_replay`, `idempotency_conflict`, `contention`, `unknown_seats`,
`show_not_found`, `unknown_user` or `invalid_request`. A replay returns `201`
but books nothing, so it counts as a decline and not as a confirmation.

**How the metrics stay consistent:**
- Every reserve request is counted exactly once: either as confirmed or under
  one decline reason. The only exception is a server error, which appears only
  in the HTTP metrics.
- The seat gauges are read from the database on each scrape, using the same
  availability rule as `GET /shows/{id}`, so they always match the API. One
  query gives one snapshot, so available + held + confirmed equals capacity in
  every scrape.
- The gauges use their own two-connection database pool, so a burst that takes
  every connection in the main pool can't stop them from being read.

**Verified under a 20,000-request burst:**
- Every counter delta exactly matched what clients observed: 805 confirmed,
  16,695 seat-taken, 300 per-user-limit, 1,800 replays and 400 idempotency
  conflicts, which adds up to all 20,000 requests.
- The confirmed delta equalled the new confirmed rows in the database.
- The HTTP metrics matched the status codes clients received.
- In all 783 scrapes taken during the burst, the seat gauges were readable and
  added up to capacity.

The counters start at zero each time the app starts, and with several app
instances you sum them across instances. Measure them as a change over a
window, for example `increase(booking_reservations_confirmed_total[5m])`.
`/metrics` is public; put it behind your network or an auth proxy in
production.

## How double-selling is prevented

The decision about who gets a seat happens in a single SQL statement
(`holdSeats` in [internal/api/reserve.go](internal/api/reserve.go)). It locks
only the requested seats that are still free, in seat-id order, and marks them
held. A hold whose time has run out counts as free, so expiry needs no
background job.

- **One winner per seat.** A request that waited on a seat's lock re-checks the
  seat after the winner commits and skips it. Every loser gets a `409`, never a
  `500`.
- **No deadlocks.** Every request locks seats in the same order.
- **Safe retries.** The idempotency key is the reservation's primary key. A
  retry with the same request returns the original reservation; reusing the
  key with different seats returns `409`.
- **Per-user limit.** A lock per user and show stops one user's parallel
  requests from going over `limit_per_user` together.

## Testing

- **Unit tests** cover the seat-suggestion logic: `go test ./...`.
- **Integration tests:** 148 black-box checks run against the Docker stack.
  They cover validation, forged and expired tokens, idempotency, limits,
  expired holds, cancellation and concurrency.
- **Load test:** 20,000 requests fired at once at a fresh show produced:
  - exactly one `201` per contested seat;
  - zero `5xx` responses;
  - available + held + confirmed equal to the total seat count throughout;
  - every user within their seat limit.

## Deploying to Render

Render builds the `Dockerfile` but ignores `docker-compose.yml`, so it won't
create the database for you.

1. Create a Postgres database on Render.
2. Create a web service from this repo with the Docker runtime.
3. Set `DATABASE_URL` to the database's internal URL and add `jwt_secret`.
4. Set the health check path to `/health/ready`.

## Project layout

```
main.go                  startup, config, graceful shutdown
internal/api/            HTTP handlers (users, shows, reserve, cancel, health, metrics)
internal/auth/           JWT issue/verify and auth middleware
internal/config/         environment variables
internal/seatmap/        seat-suggestion logic
internal/store/          schema and startup migration
docs/API.md              endpoint reference
writeup.md               design notes and how the project was built
```

## Logging

Logs are JSON on stdout, one object per line. Every request gets an `X-Request-ID` (a caller-supplied value of up to 64 safe characters is kept, otherwise one is generated). It is returned in the response header and appears as `request_id` on every log line for that request, including the access line (`method`, `route`, `status`, `duration_ms`, `user_id` when authenticated).

To trace one failing request: `docker compose logs app | grep <request_id>`. On a hosted platform, use its log viewer with the same filter.
