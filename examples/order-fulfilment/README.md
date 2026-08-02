# Order fulfilment: charging a card exactly once

An order reserves stock, charges a card, and ships. The charge is the step that must not
happen twice, and the example exists to show what genroc does when a worker dies with that
request in flight.

```
place-order
  reserve   fetch  POST /inventory/reserve      retryable — idempotent per order_ref
              ├─ http.409 ─▶ out_of_stock ─▶ raise out_of_stock
              └─ catch-all, 3 retries
  charge    fetch  POST /payments/charge        only_once: true
              ├─ pre.error / pre.timeout, 3 retries   never left → cannot double-charge
              ├─ only_once.interrupted ─▶ reconcile   nothing came back → go ask
              └─ http.402 ─▶ declined ─▶ release stock ─▶ raise payment_declined
  ship      fetch  POST /shipping/dispatch
  reconcile fetch  GET  /payments/by-key/{order_ref}   the system of record
              ├─ charged      ─▶ ship        the money already moved; do not charge again
              └─ not charged  ─▶ charge      deliberate re-entry, now known to be safe
```

## The problem `only_once` solves

A worker claims the instance, sends the charge, and the machine dies before the response
arrives. The row's lease expires and another worker picks it up. What should it do?

It cannot know whether the card was charged. The request left; nothing came back. Retrying
might double-charge; not retrying might silently drop the order. **Both choices are wrong,
and no amount of engine cleverness fixes that** — the information simply is not in the
engine.

`only_once: true` makes the engine stop guessing. It will never re-run the task on its own.
Instead the next claim raises **`only_once.interrupted`**, the one engine-produced code
`on_error` can catch, and hands the decision to the definition:

```yaml
on_error:
  - code: [only_once.interrupted]
    goto: $reconcile
```

Uncaught, it is an ordinary terminal failure — the safe default. Caught, it becomes the
one thing that *can* resolve the question: asking the system that actually knows.

## Reconciliation is the whole point

`reconcile` is a read-only lookup keyed by the same idempotency key the charge used:

```yaml
- id: reconcile
  action:
    type: fetch
    method: GET
    url: "${ input.base_url }/payments/by-key/${ input.order_ref }"
  switch:
    - case: "self.output.charged == true"
      goto: $ship
    - goto: $charge
```

Both branches matter:

- **The charge landed** → skip it entirely and ship. The order completes with no
  `outputs.charge` at all, because that task's action never returned.
- **It did not** → `goto: $charge`. The engine refuses to repeat an `only_once` call by
  itself, but it does not stand in the way of an **authored** re-entry: the definition has
  now established that re-sending is safe, and that is a claim only the definition can make.

This is why `order_ref` doubles as the payment idempotency key. Reconciliation tells you
whether to re-send; the key is what makes re-sending harmless if the answer races.

## Which retries `only_once` accepts

Registration enforces three tiers, per `on_error` pattern:

| Pattern | Retries allowed? |
|---|---|
| Only `pre.*` (the request never left) | **yes**, on its own |
| Anything else | only with `not_reached: true`, **and** naming exact codes — a wildcard is not an assertion |
| `only_once.interrupted`, `http.timeout`, `external.timeout` | **never**, however named |

That last row is the *unknowable set*: the errors where the request left and nothing came
back. `not_reached: true` cannot override them, because that flag asserts what an error
*means* — a claim you can only make about an error that returned.

So this example's first rule is the only unqualified retry it is allowed:

```yaml
- code: [pre.error, pre.timeout]
  retry: 3
```

Try adding `- code: [http.500], retry: 1` to the charge task and re-applying: registration
refuses it, and names the pattern. `isRetryAllowed` refuses at runtime too — validation runs
only at registration, and definitions stored before the rule keep their `on_error` verbatim.

## Compensation is ordinary routing

There is no `compensate` keyword. A declined card routes to `declined`, which releases the
reservation and then raises:

```yaml
- id: declined
  action: { type: fetch, url: "${ input.base_url }/inventory/release", ... }
  switch:
    - raise: { code: payment_declined, message: "..." }
```

Note the raise sits directly on the `switch` case: an arm either routes (`goto`) or
terminates (`raise` / `panic`), so no separate task is needed to fail. And note what
`declined` reads — `outputs.reserve.reservation_id`, still there, because a routed task
keeps its normal context.

`raised` rather than `failed` is deliberate: a declined card is an anticipated outcome a
parent process could catch, not a defect.

## Running it

Point it at any service exposing `POST /inventory/reserve`, `POST /inventory/release`,
`POST /payments/charge`, `GET /payments/by-key/{ref}` and `POST /shipping/dispatch`:

```sh
genctl apply -f order.genroc.yaml
genctl run place-order --input '{
  "base_url": "http://localhost:9000",
  "order_ref": "ord-1",
  "customer": "cust-1",
  "amount_cents": 2500,
  "sku": "widget"
}'
```

To see the interruption path, kill the server while `/payments/charge` is unanswered, then
start it again and watch the instance route to `reconcile`:

```sh
genctl logs <instance-id>
```

## Automated test

[`tests/integration/examples_order_test.ts`](../../tests/integration/examples_order_test.ts)
loads this file verbatim and covers all five outcomes — shipped, declined, out of stock, and
**both** interrupted paths. The interruption is not simulated: the test starts a real genroc
worker, waits until the charge request has actually arrived at the mock, `SIGKILL`s the
worker, starts a second one, and expires the lease. It then asserts the charge was attempted
**once** when reconciliation says the money moved, and **twice** when it says it did not.
Run it with `make test-int`.
