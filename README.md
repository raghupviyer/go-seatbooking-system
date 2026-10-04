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

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health/live` | No | Liveness |
| `GET` | `/health/ready` | No | Readiness (checks the database) |
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
internal/api/            HTTP handlers (users, shows, reserve, cancel, health)
internal/auth/           JWT issue/verify and auth middleware
internal/config/         environment variables
internal/seatmap/        seat-suggestion logic
internal/store/          schema and startup migration
docs/API.md              endpoint reference
writeup.md               design notes and how the project was built
```
