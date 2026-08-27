# Resource limits and readiness

Status: **shipped 2026-08-02**.

Touches: `internal/transport/transport.go`, `internal/api/server.go`,
`internal/api/handlers_health.go`, `internal/api/errors.go`, `internal/db/db.go`,
`internal/engine/engine.go`, `internal/errcode/errcode.go`.

Five limits that were absent, and one endpoint that lets a supervisor see the result. They
are grouped into one document because they share a premise: **a worker is not a request
handler.** A worker holds leases on every instance it claimed, so a fault that would cost
a stateless server one request costs genroc every instance that worker was advancing —
until each of those leases expires and a peer takes it over.

## 1. A fetch response was read without a size limit

`sendHTTP` decoded the response body straight into memory. A remote endpoint answering
with a multi-gigabyte body — or an endless one — OOMs the worker process.

The blast radius is what makes this the most severe of the five. The worker dies holding
leases on up to `--max-concurrent` instances (default 200). None of them can be picked up
until their leases lapse, and the OOM is deterministic: the supervisor restarts the worker,
the same instance is claimed again, the same endpoint is called again, and it dies again.
A single misbehaving endpoint is a crash loop that also stalls 199 unrelated processes.

**Shipped:** a `MaxResponseBytes` cap of 8 MiB, reported as the catchable error code
`output.too_large`.

Two choices worth recording:

- **8 MiB, as a constant rather than a flag.** A value externalizes to the object store at
  2 KiB (`contextObjectThreshold`), so 8 MiB is three orders of magnitude past the point
  where a result is considered large. This is a safety limit, not a tuning knob: an
  endpoint answering with more than 8 MiB of JSON is a fault in that endpoint, and a flag
  would invite raising it rather than fixing it. Making it configurable later is a
  one-line change if a real workload needs it.
- **A new code, not `output.parse`.** An oversized body and an unparseable one call for
  different fixes, and `output.parse` would send the reader to a JSON validator for a
  response that is usually valid JSON. `output.too_large` sits alongside `output.parse` in
  the "a response **did** arrive" family of
  [only-once-interrupted.md](only-once-interrupted.md): the request left, the remote
  answered, and the size is evidence about the remote's behaviour that a definition can
  legitimately reason about. It is therefore **not** in the unknowable set, and an
  `only_once` task may retry it under `not_reached: true` like any other returned error.

### Detecting the overflow

The allowance is `MaxResponseBytes + 1`, and draining it *is* the detection:

```go
limited := &io.LimitedReader{R: resp.Body, N: MaxResponseBytes + 1}
err = numeric.DecodeReader(limited, &b)
if limited.N <= 0 { /* too large */ }
```

The check runs before the decode error is consulted, because an oversized body may equally
well parse (a huge but well-formed value) or fail (a value truncated mid-token) — and when
it fails, the parse error is a *consequence* of the truncation. Reporting "invalid JSON"
for a response the remote sent correctly points at the wrong system.

A plain `io.LimitReader` cannot express this: it yields EOF at the limit, which is
indistinguishable from a body that simply ended, so an oversized response would be reported
as a parse error or — worse, for a body that happens to be a valid prefix — silently
truncated and accepted.

## 2. `http.DefaultClient` capped connection reuse at 2 per host

`http.DefaultTransport` sets `MaxIdleConnsPerHost` to 2. Every fetch beyond the second
concurrent call to one endpoint re-dialled, and on HTTPS re-ran the TLS handshake. For a
system whose entire workload is HTTP calls, usually to a small number of endpoints, that is
a per-call cost paid on almost every call.

**Shipped:** a package-level client with `MaxIdleConnsPerHost` at 64 and `MaxIdleConns` at
512.

It deliberately sets **no `Client.Timeout`**. The per-attempt budget is the caller's
context deadline, resolved per attempt by `Engine.fetchTimeout` from the task's declared
`timeout`. A `Client.Timeout` would silently cap that: a task declaring `timeout: 5m` would
be cut off at whatever the client said, and the resulting error would be classified
`http.timeout` — an unknowable code, which on an `only_once` task can never be retried. A
task would become permanently unretryable because of a default it never set.

## 3. Retry backoff had no jitter, and overflowed

`retryDelay` was `2^attempt` seconds capped at 5 minutes, with no randomization.

**Thundering herd.** Every instance that failed against the same outage retried at exactly
1s, 2s, 4s… from its own failure. Because the poll loop already aligns wakeups, those
cohorts converge rather than spread, and the recovering endpoint is hit by the whole
backlog at once.

**Overflow.** `time.Duration` is int64 *nanoseconds*, so `time.Duration(1<<attempt) *
time.Second` overflows the multiplication at attempt 34 — long before the shift itself
would overflow. Measured against the old formula: attempt 33 was the last correct value,
attempt 34 returned roughly *minus forty years*, and attempt 62 and above returned a flat
`0s`. A negative or zero delay is not a long wait, it is **no backoff at all** — a hot
retry loop against an endpoint that is already failing. `retries` has no upper bound in
validation, so reaching those attempt counts needs nothing but a definition that asks for
them.

**Shipped:** the exponent is clamped at 9 (2^9s is already past the 5-minute cap, so this
changes no delay the un-clamped formula produced), and the result is jittered:

```go
return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
```

Jitter is applied to the **upper half** of the window, not the full window. Full jitter
(`rand[0, d]`) halves the expected backoff, which weakens the property backoff exists for;
equal jitter keeps the growth curve while still spreading a cohort. The important
consequence either way is that jitter only ever **shortens** the nominal delay, so the
5-minute cap remains a true ceiling — and a test that advances the clock by the nominal
amount still expires the timer, which is how the existing retry suite keeps working
unchanged.

## 4. The HTTP listener had no connection timeouts

`&http.Server{Addr: addr, Handler: mux}` sets no `ReadHeaderTimeout`, `ReadTimeout` or
`IdleTimeout`, and no cap on a request body.

**Shipped:** `ReadHeaderTimeout` 10s, `ReadTimeout` 60s, `IdleTimeout` 120s, and
`http.MaxBytesReader` at 10 MiB applied in the route wrapper, so it covers every action
including the ones with a custom `fromHTTP`.

**There is deliberately no `WriteTimeout`.** `POST /tick` blocks until every instance it
claimed has finished advancing, which is unbounded by design — it is how the manual-tick
mode used by tests and by `-poll 0` deployments works. Any `WriteTimeout` short enough to
defend against a slow reader would also sever a legitimate long tick. `ReadHeaderTimeout`
is the one that matters for an unauthenticated listener anyway: it is what bounds a
connection that opens and then sends nothing, which is the slowloris shape.

### The drain has to be awaited, not just bounded

The first version of this change bounded `srv.Shutdown` with a 15s context but left it in a
detached goroutine. That is close to useless, and the reason is worth recording because it
is easy to reintroduce:

`ListenAndServe` returns `http.ErrServerClosed` the moment `Shutdown` closes the listener —
*before* in-flight requests finish. So `ListenHTTP` returned immediately, `main`'s
`WaitGroup` completed, and the process exited while requests were still being served. The
shutdown was graceful in name only, and the bound on `Shutdown` guarded a goroutine nobody
was waiting for.

`ListenHTTP` now waits on the shutdown goroutine before returning. A bind failure must skip
that wait: it happens before the context is ever cancelled, so the goroutine is still parked
on `ctx.Done()` and waiting for it would hang the process on a misconfigured port —
`errors.Is(err, http.ErrServerClosed)` is what separates the two paths.

## 5. There was no readiness endpoint

Nothing let a supervisor ask whether a worker was serving. `docker-compose.yml` and the
Postgres mode both imply container deployment, where the absence of a probe target means a
worker with an unreachable database stays in rotation indefinitely.

**Shipped:** `GET /healthz`, returning `HealthResp` and a new API code `CodeUnavailable`
rendering as 503.

The verdict keys on **exactly one question** — can this worker reach its database — because
that is the only failure the caller can act on by routing elsewhere. Everything else in the
body is operator context.

`lease_age_ms` is reported but **not judged**. It grows during exactly the conditions
`leaseGate` exists to recover from (see [lease-fencing.md](lease-fencing.md)): a suspended
host, a throttled container, a transient database stall. The engine repairs its own leases
and declines takeovers for one lease period, and comes back on its own. Failing the probe
on a stale lease age would restart workers that were seconds from recovering, and restart
is the one action that makes the situation worse — it discards the in-flight advances the
gate was protecting.

The handler reaches its verdict **without consulting the engine**, which is what lets a
worker whose database is gone still answer the probe rather than fault inside it.
`TestHealth_ReportsUnavailableWhenTheDatabaseIsGone` passes a nil engine to hold that
property in place.

## Still open

- **TCP and UDS have no per-message limit.** `handleConn` decodes a persistent stream of
  envelopes off the socket; bounding a single message means introducing framing, which is a
  protocol change rather than a limit. Both transports are opt-in via flag, unlike the HTTP
  listener.
- **`retries` has no upper bound at registration.** The overflow it used to cause is fixed
  in `retryDelay` (`internal/engine/backoff.go`), so a large value is now merely a
  long-running retry loop — the author's
  choice — but nothing tells them the delay stops growing after attempt 9.
- **No metrics.** `/healthz` answers "is this worker serving"; it does not answer "how many
  instances are in flight, how deep is the backlog, how often are leases being taken over".
