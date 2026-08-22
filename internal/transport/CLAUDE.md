# internal/transport

Design and the failures these prevent: [specs/resource-limits.md](../../specs/resource-limits.md).
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

## `pre.*` requires evidence, and `sendHTTP` is the only thing that has it

`ClassifyGoError` may return `pre.*` **only** for a failure `sendHTTP` wrapped in `notSent`.
That prefix is not a diagnosis, it is an assertion the engine acts on: `IsNotReached` lets
`isRetryAllowed` retry the call even on an `only_once` task. A reset or an `EOF` arriving
while the client awaits a response looks identical, at the client, to a remote that read the
request, acted on it, and died answering — so it classifies `http.disconnected`, which is in
`errcode.Unknowable()`.

The evidence is `httptrace`, not the shape of the error. Matching on `*net.OpError.Op` was
the previous rule and it was unsound in the expensive direction: the open-ended set of
post-write errors all fell through to `pre.error`. The four genuine not-reached failures —
DNS, refused, unreachable, TLS — have four unrelated error shapes, so a whitelist of them is
narrower than reality, and what it misses lands in the unknowable set where `not_reached`
cannot buy the retry back.

Five things must not change:

1. **`GotConn`, not `WroteRequest`, is what the mark rests on.** `GotConn` is delivered
   before the request reaches the write goroutine, so "no connection was acquired" is a
   stable fact once `Do` returns — and no connection means no bytes, whatever the write
   goroutine is doing. `WroteRequest` fires from that goroutine and races the return, so
   reading it alone would make `pre.*` a claim about scheduling. It stays in the trace
   because the disjunction can only widen the answer. Both are latched: the transport
   retries internally, and any attempt that got a connection counts.
2. **The traced context must reach `c.Do`.** This fails silently in the *unsafe* direction:
   rebuild the request on a context without the hooks and the flag stays false forever, so
   every failure — resets included — becomes `pre.*`.
   `TestClassifyGoError_PreOnlyWhenTheRequestNeverLeft` catches it; its reset, close and
   mid-write-deadline cases go red the moment the trace stops arriving.
3. **`sendHTTP` wraps `doHTTP` for no other reason.** Every early return in `doHTTP` happens
   before the write, so one check at the boundary marks them all — a new early return cannot
   silently inherit the wrong default. Do not move the body back up.
4. **Any write attempt counts as sent**, including `WroteRequest` with a non-nil `Err`. A
   partial request is bytes on the wire; a conforming server will not dispatch it, but
   `only_once` is not the place to bet on the remote conforming.
5. **Both write paths are covered.** h2 serialises through HEADERS frames rather than
   `Request.write`, and the shared transport keeps `ForceAttemptHTTP2`, so every HTTPS
   endpoint takes that path. `TestClassifyGoError_HTTP2RequestThatReachedTheRemote` pins it;
   without it, an h2 regression in the hook would silently reclassify the whole path.

The client is a parameter of `sendHTTP` rather than read from the package var **so that
that test can exist** — h2 needs the test server's own client to trust its certificate.

The remote cannot argue with any of this: the mark is read off genroc's own transport, never
off anything the peer says, so a peer can only push the answer toward *unknowable* (accept,
read, reset) — never toward `pre.*`. The assumption underneath is that no request byte can
precede a connection, which TLS 1.3 **0-RTT early data would break**: the request would ride
with the handshake, so a handshake failure would report `pre.*` for a request the server
received and may replay. Go gates every early-data path on `c.quic != nil` (assigned only by
`tls.QUICClient`), and `tls.Conn.Write` handshakes first, so `net/http` cannot reach it
today. Re-check this before adopting HTTP/3, or if Go ships client-side 0-RTT over TCP.

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

## The retry backoff is not here

It moved to `internal/engine/backoff.go` when the curve became authorable per `on_error`
rule: it takes its base, factor and ceiling from a `model.Retry`, which this package must
not depend on. The jitter and overflow invariants moved with it — see
[internal/engine/CLAUDE.md](../engine/CLAUDE.md).
