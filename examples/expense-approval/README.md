# Expense approval: a process that waits on a human

An expense is submitted, someone reviews it, and it is either reimbursed or rejected. If
nobody reviews it in time it escalates to a manager. The whole example is one definition —
the waiting is a task type, not an architecture.

```
expense-approval
  notify          fetch     POST /notify                 tell the reviewer
  review          external  parks until someone answers   timeout_ms: 1h
                    ├─ approved      ─▶ pay
                    ├─ not approved  ─▶ reject ─▶ raise expense_rejected
                    └─ external.timeout ─▶ escalate
  pay             fetch     POST /reimburse
  escalate        fetch     POST /notify                 tell the manager
  manager_review  external  parks with NO timeout         a decision, not a default
                    ├─ approved      ─▶ pay
                    └─ not approved  ─▶ reject
```

## What `external` actually is

`external` parks the instance until an outside caller submits a result. Two things make it
different from "poll a queue in a loop":

- **No worker is held.** The instance is a row in `waiting` state. A process can sit here
  for days at zero cost, and ten thousand of them cost the same as one.
- **The park is durable.** Restart the server and the parked instances are still parked —
  there is no goroutine, timer or in-memory subscription to lose.

## The queue is the resolver's entire view

`GET /external-tasks` lists what is waiting. Each entry carries exactly three things: the
**input snapshot** the task published, the **`result_schema`** the answer must satisfy, and
a **token**.

```json
{
  "token": "019fc30f-….019fc30f-…",
  "process": "expense-approval",
  "task_id": "review",
  "input": { "requester": "ada", "amount_cents": 4200, "purpose": "conference ticket" },
  "result_schema": { "type": "object", "properties": { … }, "required": ["approved", "reviewer"] },
  "waiting_since": "2026-08-02T17:20:13Z"
}
```

It never exposes the process context. A reviewer sees the expense and nothing else — not the
other tasks' outputs, not the config, not the instance id. That is why the task declares its
own `input` rather than the resolver reading from the instance:

```yaml
action:
  type: external
  input:
    requester: "$: input.requester"
    amount_cents: "$: input.amount_cents"
    purpose: "$: input.purpose"
```

## The answer is checked before it lands

`result_schema` is a contract published *before* the answer is written, so a malformed
submission is refused at the API and the task stays parked for a valid one:

```sh
# refused — approved must be a boolean, and reviewer is required
genctl resolve <token> --result '{"approved": "yes"}'
```

This is the part that is hard to get right by hand. The process resumes only with data that
already conforms, so the `switch` reading `self.output.approved` cannot be handed a string.

## Two ways to answer

**By token**, from the queue — for a reviewer UI that lists what is pending:

```sh
genctl external-tasks --process expense-approval
genctl resolve <token> --result '{"approved": true, "reviewer": "alice"}'
```

**By instance and task id**, when the caller already knows which run it is answering — a
webhook that carries your own correlation id, for instance:

```sh
genctl signal <instance-id> --task review --result '{"approved": true, "reviewer": "alice"}'
```

`signal` also buffers: deliver a result before the task has armed and it is held FIFO until
the task next parks, which removes a race a token-only API would force you to handle.

## Timeouts, and why the review window is a constant

`timeout_ms` on the task bounds the wait, and expiring raises the catchable
`external.timeout` — so escalation is ordinary error routing, not a special mechanism:

```yaml
timeout_ms: 3600000
on_error:
  - code: [external.timeout]
    goto: $escalate
```

**`timeout_ms` is a static integer — it cannot be an expression.** The review window is
therefore fixed by the definition, not supplied per instance. If you need a caller-supplied
deadline today, the way to express it is a `delay` task (whose `for` / `until` *are*
expressions) racing the approval in a sibling branch — considerably more machinery than
this example needs.

The second park deliberately has **no** timeout. Once a request has already been escalated,
timing out again would mean deciding by default, and the point of escalation is to get a
decision.

## Notification is not genroc's job

`notify` is a plain `fetch`. genroc parks the process; it does not deliver email, Slack or
tickets. Keeping the two separate means only that one task knows about your notification
system, and swapping it changes nothing else in the definition.

## Running it

Point it at any service exposing `POST /notify` and `POST /reimburse`:

```sh
genctl apply -f approval.genroc.yaml
genctl run expense-approval --input '{
  "base_url": "http://localhost:9000",
  "requester": "ada",
  "amount_cents": 4200,
  "purpose": "conference ticket"
}'

genctl external-tasks --process expense-approval
genctl resolve <token> --result '{"approved": true, "reviewer": "alice"}'
```

## Automated test

[`tests/integration/examples_approval_test.ts`](../../tests/integration/examples_approval_test.ts)
loads this file verbatim and covers approval by token, rejection by `signal` (asserting
`raised`, not `failed`), a schema-violating submission that is refused while the task stays
parked, and the timeout path — which runs on its own tick-only server so that shifting the
clock past the one-hour window cannot disturb anything else. Run it with `make test-int`.
