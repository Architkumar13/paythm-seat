# Write-up

## Atomic decision

The system of record is one Postgres primary. A seat is one row in `seats` with primary key `(show_id, seat_label)`, so a second copy of A12 cannot be inserted. Status is `available`, `held`, or `confirmed`.

`Reserve` runs in a single `READ COMMITTED` transaction. The lock order is always:

1. `idempotency_keys` row for `(user_id, idempotency_key)`
2. `user_show_counts` row for `(user_id, show_id)`
3. `seats` rows, one `SELECT ... FOR UPDATE` per label, in ascending `seat_label` order

After those locks are held, each seat is updated with:

```sql
UPDATE seats
SET status = 'confirmed', reservation_id = $reservation, user_id = $user
WHERE show_id = $show AND seat_label = $label AND status = 'available'
```

The row lock makes the check and the update one atomic step. The `status = 'available'` predicate is the backstop: a second transaction blocks on the lock, then updates zero rows. Losers roll back. That is why a hot-seat storm returns one `201` and the rest `409 seat_taken`, not two owners and not a `500`.

The per-user limit uses the same idea on `user_show_counts`. The row is inserted if missing, locked with `FOR UPDATE`, and incremented only when `confirmed_seats + n <= per_user_limit`. Two parallel reserves for the same user serialize on that row, so ten requests against a limit of 4 end at 4.

Multi-seat requests are all-or-nothing. Every requested label is locked in sorted order and checked before any of them is updated. If one is gone, the transaction returns `409` and undoes the quota change. Two buyers asking for overlapping sets cannot deadlock: both acquire seat locks in the same global label order, and both acquire the quota lock before any seat lock. Cancel uses that same quota-then-seats order after it locks the reservation row.

`READ COMMITTED` plus explicit locks is deliberate. `SERIALIZABLE` would abort one side of a hot-seat fight and turn a normal decline into a retry storm. Deadlock (`40P01`) and serialization failure (`40001`) are retried inside the process up to three times. They are not returned to the client.

## Idempotency

The key lives in `idempotency_keys`, unique on `(user_id, idempotency_key)`. The request fingerprint is `sha256(show_id + sorted seat labels)`.

The insert is `INSERT ... ON CONFLICT DO NOTHING`. Postgres makes the loser of that unique index wait until the winner commits or rolls back, so two retries of the same key cannot both book. The winner stores a terminal `result_code` before commit: `confirmed` plus `reservation_id`, or a decline such as `seat_taken` or `per_user_limit`. Quota and seat changes sit after a savepoint. A decline rolls back to that savepoint, then commits only the idempotency row. A crash before commit leaves no key and no seat change, so the client can retry safely.

A retry with the same hash returns the stored reservation (`200`, `Idempotent-Replayed: true`) or the stored decline (`409`/`400` with the same header). It does not increment the quota and it does not confirm a seat, including after the original seat has been cancelled and is free again. A new key is required for a new attempt. The same key with a different seat set or a different show returns `409 idempotency_mismatch` and does not overwrite the stored result.

Invalid JSON, duplicate labels, and a missing key are `400` and are not stored, so the client can fix the body and reuse the key.

## Holds and expiry

Reserve confirms immediately. The `201` body in the brief has `"status": "confirmed"`, and there is no payment-authorization step that would need a hold window. `held` is part of the seat check constraint and of every show response, and it stays 0.

Release is `POST /reservations/{id}/cancel`. The reservation row is locked, the caller must be `user_id`, then the quota row and the seat rows are locked in the order above. Each seat is cleared only by:

```sql
UPDATE seats
SET status = 'available', reservation_id = NULL, user_id = NULL
WHERE show_id = $show AND seat_label = $label
  AND reservation_id = $reservation AND status = 'confirmed'
```

A cancel therefore cannot wipe a seat that now points at a newer reservation. A second cancel sees `status = cancelled` and returns the same row without touching seats. The quota is decremented in the same transaction, so the owner can book again, up to the limit, with a new idempotency key.

## Consistency and availability

This is a CP design around one primary. A confirmed reservation is committed with synchronous commit before the `201`. There is no cache and no second writer in front of `seats`.

If the process cannot reach Postgres, `/health/ready` returns `503` and the platform should stop routing. A reserve that fails because the database is unreachable is a `500`: the server does not guess `409` or `201` for a result it could not record. During a partition from the database the service is unavailable rather than double-selling. Reads of a show are one SQL statement, so the seat list is a single snapshot. Gauges on `/metrics` are the same table.

## Observability

Page on:

- `/health/ready` failing. The dependency is down and the process must fail closed.
- Any `5xx` (`http_responses_total`). A decline is a 4xx. A 500 means we lost the plot.
- `seats_available + seats_held + seats_confirmed != seats_total` for a show. That is the reconciliation invariant, and the gauges are read from Postgres at scrape time so they match `GET /shows/{id}`.
- `reservation_db_retries_total` climbing. A retry means a deadlock or a serialization failure. The request still succeeds or declines cleanly, but a rising rate means the lock order or the load has changed.
- Confirmed rate stuck at 0 during a known on-sale. The counters are process-local. Gauges survive a restart; counters reset, which Prometheus treats as a counter reset.

Logs are JSON with `request_id`, method, path, status, and duration. Confirm, decline, replay, and cancel lines include `user_id`, `show_id`, and the reason. A body `user_id` that does not match the token is logged as ignored and is not used.

`reservations_confirmed_total` counts reservations, not seats. A two-seat booking is one confirmation and two `seats_confirmed`.

## AI usage

I used Grok (xAI) in Grok Build as the implementation assistant for this take-home. I set the constraints: no double-sell, per-user limit under parallel requests, idempotency that does not move a second seat, all-or-nothing multi-seat, zero 5xx on declines, integer paise, a public deploy, and a burst that checks the live service.

The assistant wrote the Go service, the SQL, the tests, the container, and this write-up in one session. The race-free mechanism was chosen there and is the one above: Postgres row locks in a fixed order, a conditional `UPDATE ... WHERE status = 'available'`, a quota row updated in the same transaction, and an idempotency unique key that stores the first outcome. I can walk through `internal/store/reserve.go` and extend it live. I would not describe this as code I typed by hand.

## What I would do next

- A real hold: `held` until a payment webhook confirms or a deadline expires, with the same conditional update so expiry cannot release a seat that was confirmed in the meantime.
- An admission queue in front of the hottest shows so 20k clients do not all pin database connections. The transaction above stays the source of truth; the queue only cuts wait time.
- Idempotency-row expiry, so keys do not grow forever.
- One writer primary and read replicas for `GET /shows/{id}`. Replicas can lag; gauges and the sale stay on the primary.
- A periodic reconciliation job that pages when confirmed seats, reservation rows, and `user_show_counts` diverge. The integration test already runs those queries.
- Move `reservations_confirmed_total` into the database if we run more than one app instance. Scraped gauges are already global.
