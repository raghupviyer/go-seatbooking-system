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

## Burst test (on-sale stampede)

One command reproduces an on-sale stampede against any running instance,
local or deployed. It needs only Go and the app's base URL:

```bash
# any of these; BASE_URL defaults to http://localhost:8080 for make
make burst BASE_URL=https://your-app.onrender.com
./burst.sh https://your-app.onrender.com
go run ./cmd/burst https://your-app.onrender.com      # works on Windows too
```

Tune it with flags, for example `./burst.sh <BASE_URL> -users 1000 -rows 20`,
or `make burst BASE_URL=... BURST_FLAGS="-users 1000"`. Run
`go run ./cmd/burst -h` for the full list (`-users`, `-rows`, `-cols`,
`-limit`, `-retry-pct`, `-setup-concurrency`, `-timeout`, `-seed`).

What it does:

1. Registers and logs in N users (default 300). Each run uses fresh user names
   and shows, so it can be repeated against the same deployment.
2. **Hot-seat storm:** every user reserves the same seat at the same instant.
3. **Stampede:** every user reserves 1-4 seats at once in a 10×10 show, skewed
   to the front rows. 10% of requests are also sent twice under the same
   idempotency key, like a double click.
4. Prints the outcome distribution (confirmed, declined by reason, 5xx) with
   latency percentiles, then reconciles the clients' results against
   `GET /shows/{id}`.

Example output:

```
== hot-seat storm (same seat, all users) ==
500 requests in 1.12s (446 req/s)   latency p50=675ms p95=1.051s p99=1.076s max=1.078s
  declined: seat_taken                499   99.8%
  confirmed                             1    0.2%

== stampede (random seats, skewed to front rows) ==
547 requests in 1.238s (442 req/s)   latency p50=478ms p95=922ms p99=1.033s max=1.072s
  declined: seat_taken                496   90.7%
  confirmed                            44    8.0%
  idempotent_replay                     7    1.3%

== reconciliation ==
hot-seat show  clients told: 1 reservations = 1 seats | server: confirmed=1 held=0 available=0 total=1
stampede show  clients told: 44 reservations = 83 seats | server: confirmed=83 held=0 available=17 total=100

RESULT: PASS - no seat sold twice, counts reconcile, no 5xx
```

The run fails (exit code `1`) if any of these checks fails:

- a seat was confirmed to two clients;
- the hot seat had other than exactly one winner;
- the seats confirmed to clients differ from the server's `confirmed` count;
- a user went over `limit_per_user`;
- seats are still `held`, or `available + held + confirmed` doesn't equal the total;
- any request got a 5xx or a transport error.

On a free hosting tier, setup takes longer because bcrypt runs once per user.
Lower `-users` or `-setup-concurrency` if registration times out.

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
cmd/burst/               on-sale stampede load script (make burst / ./burst.sh)
docs/API.md              endpoint reference
WRITEUP.md               design decisions and trade-offs
docs/ai-prompt.md        the prompt used to build the first version
```

## Logging

Logs are JSON on stdout, one object per line. Every request gets an `X-Request-ID` (a caller-supplied value of up to 64 safe characters is kept, otherwise one is generated). It is returned in the response header and appears as `request_id` on every log line for that request, including the access line (`method`, `route`, `status`, `duration_ms`, `user_id` when authenticated).

To trace one failing request: `docker compose logs app | grep <request_id>`. On a hosted platform, use its log viewer with the same filter.
