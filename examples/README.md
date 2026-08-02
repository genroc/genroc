# Examples

Each directory is a working set of definitions plus a README explaining the pattern it is
built around. Every example is also an executable integration test, applied verbatim from
these files — so if a README drifts from the YAML, the suite fails.

| Example | Pattern | Covers |
|---|---|---|
| [order-fulfilment/](order-fulfilment/) | charging a card exactly once | `only_once`, `only_once.interrupted`, reconciliation against a system of record, the retry tiers, compensation |
| [expense-approval/](expense-approval/) | a process that waits on a human | `external` tasks, the resolve queue and token, `signal`, `result_schema` as a published contract, `external.timeout` escalation |
| [batch-invoices/](batch-invoices/) | fan-out over a batch | `child_list`, `child_map`, `map` lambdas, and when a failed item is a *result* vs a *raise* |
| [polling-task/](polling-task/) | polling a remote job | the `unknown` type (`{}`), status-code branching via `on_error`, a structural poll loop, child→parent raise |

## Running any of them

```sh
make build
./genroc -db genroc.db          # in one terminal

genctl apply -f <files…>        # children before parents
genctl run <process> --input '{…}'
genctl get <instance-id>
genctl logs <instance-id>
```

Each README lists the HTTP endpoints its example expects, so you can point it at a stub
service.

## Running the tests

```sh
make test-int
```
