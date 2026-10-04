# Seat reservation

JSON API that sells assigned seats for one show. A seat is confirmed to at most one user, a user cannot pass the per-show limit, and a repeated idempotency key does not create a second reservation. Money is integer paise.

The sale decision is one Postgres transaction: lock the buyer's quota row, lock the requested seats in label order, and update a seat only while its status is `available`. Details are in [WRITEUP.md](WRITEUP.md).

## Run locally

```bash
docker compose up --build -d
```

The API listens on `http://localhost:8080`. Postgres is published on `localhost:5433` for tests. Local admin token: `dev-admin-token`.

```bash
curl -s http://localhost:8080/health/ready
```

Readiness returns 503 when Postgres cannot be reached. Liveness (`/health/live`) only checks the process.

## Auth

This is a demo identity provider. `POST /users` registers a username, or logs in if it already exists, and returns a bearer token. The token subject is the only user a request can act as. A `user_id` field in a body is ignored.

Creating a show requires `Authorization: Bearer $ADMIN_TOKEN`. User tokens cannot create shows. A user can cancel only their own reservations.

## API

### Create a show

`POST /shows` with the admin token.

```bash
curl -s -X POST http://localhost:8080/shows \
  -H "authorization: Bearer dev-admin-token" \
  -H "content-type: application/json" \
  -d '{"name":"friday-night","seats":["A1","A2","A3"],"price_paise":25000}'
```

`per_user_limit` is optional and defaults to 4 (maximum 50). `price_paise` is a JSON integer. Floats are rejected.

### Register

`POST /users`

```bash
curl -s -X POST http://localhost:8080/users \
  -H "content-type: application/json" \
  -d '{"username":"ada"}'
```

### Reserve

`POST /shows/{id}/reserve` with the user token. Send the idempotency key in the body or in the `Idempotency-Key` header. If both are set they must match.

```bash
curl -s -X POST http://localhost:8080/shows/SHOW_ID/reserve \
  -H "authorization: Bearer USER_TOKEN" \
  -H "content-type: application/json" \
  -d '{"seats":["A1"],"idempotency_key":"ada-attempt-1"}'
```

- `201` new confirmed reservation. `amount_paise` is `price_paise * seat count`.
- `200` with header `Idempotent-Replayed: true` when that key already confirmed a reservation. Same reservation id.
- `409` `seat_taken` when any requested seat is not available. No seat from the request changes hands (all-or-nothing).
- `409` `per_user_limit` when the user would hold more than the show limit.
- `409` `idempotency_mismatch` when the key was already used for a different show or a different set of seats. Seat order does not matter.
- `400` `unknown_seat` or `invalid_request`.
- A stored decline is replayed on the same key (`409` or `400`, plus `Idempotent-Replayed: true`) and does not take a seat later. Use a new key to try again.

### Cancel

`POST /reservations/{id}/cancel` with the owner's token. Seats return to `available`. Cancelling again is a `200` and does not release a seat that someone else has since confirmed. Another user gets `403`.

### Show state

`GET /shows/{id}` is public.

```json
{"counts":{"available":1,"held":0,"confirmed":2},"total_seats":3}
```

`available + held + confirmed == total_seats`. `held` stays 0: reserve confirms immediately, and release is an explicit cancel. Confirmed seats include `user_id`.

## Observability

- `GET /metrics` is Prometheus text. `seats_available`, `seats_held`, `seats_confirmed`, and `seats_total` are queried from Postgres on each scrape, labeled by `show_id`.
- `reservations_confirmed_total` counts new reservations, not seats and not replays.
- `reservations_declined_total{reason}` uses `seat_taken`, `per_user_limit`, `idempotency_mismatch`, `idempotent_replay`, and `unknown_seat`.
- Logs are JSON on stdout. Every line has `request_id`. The response echoes `X-Request-Id`.

```bash
docker compose logs -f app
```

## Burst

The script creates a show, storms one hot seat with many users, replays the winner, checks the per-user limit, checks all-or-nothing, cancels and rebooks, and prints reconciliation against `/metrics`.

```bash
export ADMIN_TOKEN=dev-admin-token
./burst.sh http://localhost:8080
```

Windows, or any machine with Go:

```bash
go run ./cmd/burst --base-url http://localhost:8080 --admin-token dev-admin-token
```

Defaults: 300 users, 40 in flight, 24 seats, limit 4. Raise the storm with `--users 2000 --concurrency 100`. Exit code is 1 when an invariant fails or any response is 5xx.

## Tests

```bash
docker compose up -d db
go test ./...
```

`TEST_DATABASE_URL` defaults to the compose database in the Makefile (`make test`). The integration test refuses to run unless the URL points at localhost.

## Deploy

`render.yaml` is a Render blueprint: a Docker web service and Postgres, health check `/health/ready`. In the Render dashboard choose **New → Blueprint**, point it at this repo, and apply the file. Put the service and the database in the same region.

The blueprint admin token is `paytm-demo-admin`. It is a public demo credential; registration is open and there are no real charges. Free Postgres on Render expires 30 days after creation. Free web services sleep after about 15 minutes; the burst script waits up to 90 seconds for `/health/ready` on a cold start.

```bash
export ADMIN_TOKEN=paytm-demo-admin
./burst.sh https://YOUR-SERVICE.onrender.com
```

Metrics are `https://YOUR-SERVICE.onrender.com/metrics`. Logs are the service logs in the Render dashboard (JSON lines on stdout, each with `request_id`).

## Configuration

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string |
| `ADMIN_TOKEN` | Bearer token for `POST /shows` |
| `JWT_SECRET` | HMAC secret for user tokens (30 day lifetime) |
| `PORT` | HTTP port, default `8080` |
| `DB_MAX_CONNS` | Pool size, default `20` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, or `error` |
