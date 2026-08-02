# Batch invoices: fan-out, and what a failed item means

One billing run sends many invoices. It shows both fan-out shapes and — the reason the
example exists — the difference between an item that failed and a run that failed.

```
invoice-run (parent)
  rates     child_map   ──▶ fetch-rate (usd)     a FIXED set of named branches
                        ──▶ fetch-rate (eur)     result: object keyed by name
  send_all  child_list  ──▶ send-invoice × N     one child per array element
              │                                  result: array in `over` order
              └─ on_error period_closed ─▶ halted
```

## Two fan-outs, two different questions

**`child_map`** is for a fixed set of named branches you write into the definition. The keys
are part of the source, and the result is an object keyed by them:

```yaml
action:
  type: child_map
  children:
    usd: { name: fetch-rate, input: { currency: usd }, result_schema: … }
    eur: { name: fetch-rate, input: { currency: eur }, result_schema: … }
output:
  usd: "$: self.result.usd.rate"
```

Each branch types its own `result_schema` **on the entry**, not on the action — the branches
are independent processes and need not share an output shape.

**`child_list`** is for one child per element of a runtime array. The count is not known
until the instance runs, and the result is an array in `over` order regardless of the order
the children actually finished:

```yaml
action:
  type: child_list
  name: send-invoice
  over: "$: map(input.invoices, i => {base_url: input.base_url, period: input.period, invoice_id: i.invoice_id, …})"
  result_schema: …          # one schema for the whole batch — every element is the same process
```

`over` builds each child's input, which is how the parent's own constants (`base_url`,
`period`) reach every element. A child only ever sees the input its parent hands it.

Both are concurrent, and the parent holds no worker while they run.

## The distinction the example is built around

When one invoice cannot be sent, you have to choose what that *is*:

| | shape | effect |
|---|---|---|
| **A result** | child COMPLETES with `{ok: false, reason}` | every sibling still runs; the parent collects all N |
| **Control flow** | child RAISES | the batch is abandoned; the parent routes on the first raise in slot order |

Getting this backwards is the common mistake. A child that raises on a bad invoice throws
away the other 49 results to report something the parent could simply have read.

So `send-invoice` does both, deliberately:

```yaml
on_error:
  - code: [http.422]                 # permanently unsendable — no sibling is affected
    goto: $unsendable                #   → completes with ok:false
  - code: [http.500, http.503]       # transient
    retries: 2
    goto: $unsendable
  - code: [http.423]                 # the billing period is locked — every sibling
    raise:                           #   is about to hit the same wall
      code: period_closed
```

A rule of thumb: **raise only when the remaining siblings are pointless.**

## What the parent sees when a child raises

`$error` for a fan-out is identity and code only — no child data crosses:

```yaml
- id: halted
  output:
    halted_at: "$: error.child_index"   # integer, child_list.  child_key (string) for child_map
    code: "$: error.code"
    detail: "$: error.message"
```

Two consequences worth knowing:

- **There is no partial batch output.** `outputs.send_all` is written only when every child
  completed. A raise means it is absent entirely, not half-populated.
- **Only the first raise is reported**, in slot order. If you need "which of the 10 failed
  and why", that is a *result*, not control flow — use the `{ok, reason}` convention and read
  the collected array.

The routed task keeps its normal context otherwise: `input`, `config` and every previously
completed `outputs.<id>` are readable as usual.

## Retry policy belongs in the child

`retries` is not available on a `child_list` / `child_map` task. Retrying there would mean
re-spawning the whole batch, including the children that already succeeded. Per-item retry
therefore lives inside the child, where it can retry just that item:

```yaml
- code: [http.500, http.503]
  retries: 2
  goto: $unsendable
```

The parent sees a plain success; the two 503s never reach it.

## Coalescing across branches is non-null when the branches cover every ending

`send-invoice` finishes on one of two tasks, so its output reads both:

```yaml
ok: "$: outputs.send.ok ?? outputs.unsendable.ok"
```

That infers as plain `boolean`, not `boolean|null`. `send` and `unsendable` are the
process's only terminals, so exactly one of them is always set — and the output expression
is typed **once per terminal and joined**, which is what preserves that correlation. The
parent's `result_schema` therefore declares `ok` as `boolean`.

The precision is not a special case in `??`; it falls out of the partition, so it stays
honest in both directions:

- Add a third way to end that sets neither output, and `ok` goes back to `boolean|null` —
  because it genuinely can be null.
- If a branch's own output is declared nullable, the result stays nullable too: coverage
  means the value is *present*, not that it is *non-null*.

Where you do need a fallback — an uncovered terminal, a genuinely nullable branch — a
trailing default ends the chain: `?? false`. Full design and the deliberate limit (mid-process
task contexts are still collapsed) in [docs/path-sensitive-output.md](../../docs/path-sensitive-output.md).

## One rough edge this example runs into

**There is no `len()` or `filter()`.** `map` is the only builtin, so the run reports the
collected array and leaves counting the failures to the caller. Aggregating in the definition
would need a child that folds — worth knowing before you plan a summary task.

## Running it

Point it at any service exposing `GET /rates/{currency}` and `POST /invoices/send`:

```sh
genctl apply -f invoice.genroc.yaml -f rate.genroc.yaml -f run.genroc.yaml
genctl run invoice-run --input '{
  "base_url": "http://localhost:9000",
  "period": "2026-07",
  "invoices": [
    { "invoice_id": "inv-1", "customer": "acme", "amount_cents": 12000 },
    { "invoice_id": "inv-2", "customer": "globex", "amount_cents": 4500 }
  ]
}'
```

Children are applied before the parent so its child references resolve at registration.

## Automated test

[`tests/integration/examples_batch_test.ts`](../../tests/integration/examples_batch_test.ts)
loads these files verbatim and covers all three shapes — a per-invoice failure that still
lets every sibling run (with the failed one in the *middle*, so an aborted batch would
visibly lose the third invoice), a run-wide raise caught by `child_index`, and a transient
failure absorbed by the child's own retries. Run it with `make test-int`.
