# internal/transport

Design and the failures these prevent: [docs/resource-limits.md](../../docs/resource-limits.md).
Everything below breaks silently — none of it is a compile error.

## The response cap detects overflow by draining an allowance of `N+1`

`sendHTTP` decodes through `&io.LimitedReader{N: MaxResponseBytes + 1}` and treats
`limited.N <= 0` as "the body exceeded the cap". Three things must not change:

1. **The `+1` is the detection.** At exactly `MaxResponseBytes` the reader still has one
   byte of allowance left, which is what distinguishes a body *at* the limit (accepted)
   from one *past* it (refused). Dropping it rejects responses the limit is documented to
   allow.
2. **It cannot be `io.LimitReader`.** That returns EOF at the limit, which is
   indistinguishable from a body that simply ended — so an oversized response would be
   reported as `output.parse`, or, for a body whose prefix happens to be a complete JSON
   value, silently truncated and **accepted as the task's result**.
3. **The size check runs before the decode error is consulted.** An oversized body may
   parse or fail depending on where it was cut, and when it fails the parse error is a
   consequence of the truncation. Swapping the order reports "invalid JSON" for a response
   the remote sent correctly.

`output.too_large` belongs to the "a response arrived" family, alongside `output.parse` —
**not** to `errcode.Unknowable()`. The request left and the remote answered; only the size
was refused. Adding it to the unknowable set would make it permanently unretryable on an
`only_once` task, for a call whose outcome is in fact knowable.

## The shared client must not gain a `Client.Timeout`

`client` sets no `Timeout` on purpose. The per-attempt budget is the caller's context
deadline, resolved from the task's declared `timeout` by `Engine.fetchTimeout`. A
`Client.Timeout` silently caps that — and `ClassifyGoError` maps the resulting cancellation
to `http.timeout`, which is in the unknowable set, so on an `only_once` task the task
becomes permanently unretryable because of a default nobody set.

The raised `MaxIdleConnsPerHost` is the reason the client exists at all: `http.DefaultClient`
pools 2 connections per host, so a worker re-dials and re-runs the TLS handshake for nearly
every call to the same endpoint. `TestClient_PoolsConnectionsPerHost` pins both properties,
because reverting to `http.DefaultClient` is a textual change that compiles and degrades
silently.

## `RetryDelay` jitter may only shorten

Jitter is applied to the upper half of the window (`d/2 + rand[0, d/2]`), so the returned
delay is always **≤** the nominal `2^attempt` seconds. That is load-bearing twice over: the
5-minute cap stays a true ceiling, and the clock-advancing integration tests still expire a
retry timer by advancing the nominal amount. Widening the jitter above the nominal value
breaks the second silently — as intermittent failures in tests that have nothing to do with
retries.

The exponent clamp at 9 is not cosmetic. `time.Duration` is int64 *nanoseconds*, so
`time.Duration(1<<attempt) * time.Second` overflows the multiply at attempt 34, not at 63:
it returned about minus forty years at 34 and a flat `0s` from 62 up. A zero or negative
delay is a hot retry loop against an already-failing endpoint, and `retries` has no upper
bound at registration to keep a definition out of that range.
