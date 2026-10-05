# Resource limits and readiness

Status: **Built.**

Five limits and a readiness endpoint, sharing one premise: **a worker is not a request handler.**
It holds leases on every instance it claimed (up to `--max-concurrent`, default 200), so a fault
that would cost a stateless server one request costs genroc every one of those instances until
their leases lapse. The traps that fail silently are in
[internal/transport/CLAUDE.md](../internal/transport/CLAUDE.md) and
[internal/api/CLAUDE.md](../internal/api/CLAUDE.md).

## 1. A fetch response is capped at 8 MiB

`sendHTTP` reads at most `MaxResponseBytes` (8 MiB); past it the task fails with the catchable
code `result.too_large`. Without the cap, one endpoint answering with a huge or endless body OOMs
the worker deterministically — restart, reclaim, re-call, die — stalling every instance it leased.

- **A constant, not a flag.** Values externalize at 2 KiB, so 8 MiB of JSON is a fault in the
  endpoint, and a flag would invite raising it rather than fixing it.
- **Its own code, not `result.parse`**: the fixes differ, and an oversized body is usually valid
  JSON. It is in the "a response arrived" family ([only-once-interrupted.md](only-once-interrupted.md)),
  **not** the unknowable set, so an `only_once` task may retry it under `not_reached: true`.
- **Overflow is detected by draining an allowance of `MaxResponseBytes + 1`**, checked before the
  decode error. `io.LimitReader`'s EOF at the limit is indistinguishable from a body that ended,
  so an oversized body would read as a parse error — or, as a valid prefix, be accepted truncated.

## 2. One shared HTTP client, pooling 64 connections per host

`http.DefaultTransport` keeps 2 idle connections per host, so nearly every concurrent fetch to one
endpoint re-dialled and re-ran the TLS handshake. The package client keeps 64 per host, 512 total.

It sets **no `Client.Timeout`**. The per-attempt budget is the context deadline from the task's
`timeout` (`Engine.fetchTimeout`), and a client timeout would silently cap it — classified
`http.timeout`, which is unknowable, so an `only_once` task would become unretryable because of a
default it never set.

## 3. Retry backoff is jittered and cannot overflow

The curve is [retry-policy.md](retry-policy.md)'s — `delay` grown by `factor`, stopping at
`max_delay` (`internal/engine/backoff.go`) — computed in float64, because a shifted
`time.Duration` overflows into a negative or zero delay: a hot retry loop, not a long wait.

Jitter takes the **upper half** of the window, `d/2 + rand[0, d/2]`. Without it, instances failing
against one outage retry in lockstep, and the poll loop aligns them further. Full jitter
(`rand[0, d]`) would halve the expected backoff; equal jitter keeps the curve. Jitter only
**shortens**, so `max_delay` stays a true ceiling and a test advancing the clock by the nominal
delay still fires the timer.

## 4. The HTTP listener has connection timeouts

`ReadHeaderTimeout` 10s — what bounds a connection that opens and sends nothing, the slowloris
shape — `ReadTimeout` 60s, `IdleTimeout` 120s, and a 10 MiB body cap (`http.MaxBytesReader`)
applied in the route wrapper so it covers every action, including a custom `fromHTTP`.

**No `WriteTimeout`.** `POST /tick` blocks until every instance it claimed has advanced, which is
unbounded by design (manual-tick mode, `--poll 0`, which the tests use); any write timeout short
enough to matter would sever it.

### The drain has to be awaited, not just bounded

Shutdown drains in-flight requests for up to 15s, and `ListenHTTP` waits for the drain before
returning, because `ListenAndServe` returns `http.ErrServerClosed` as soon as the listener closes —
before requests finish. A bind failure skips the wait: the shutdown goroutine is still parked on a
context nothing has cancelled.

## 5. `GET /healthz` is the readiness endpoint

It answers 503 (`CodeUnavailable`) on **exactly one question — can this worker reach its
database** — the only failure a caller acts on by routing elsewhere. The rest of `HealthResp` is
operator context.

`lease_age_ms` is reported, **not judged**: it grows during exactly what `leaseGate` recovers from
on its own — a suspended host, a throttled container, a database stall
([lease-fencing.md](lease-fencing.md)) — and a restart discards the advances the gate was
protecting.

The verdict is reached **without consulting the engine**, so a worker whose database is gone still
answers; `TestHealth_ReportsUnavailableWhenTheDatabaseIsGone` passes a nil engine to hold it.

## Open

- **Metrics** — when operating a fleet needs more than "is this worker serving": in-flight
  instances, backlog depth, the age of the oldest due `wake_at`, lease takeovers.
