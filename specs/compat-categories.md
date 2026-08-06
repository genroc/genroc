# Compat verdict categories and gating

Refines [version-compatibility.md](version-compatibility.md) §3a/§3b, which reports two
verdicts — continuation and the output contract — and leaves `genctl compat` to fold them
into one word. That fold answers the wrong question, and this records the split, the rule
that assigns a slot to a side, and how an operator says which side gates their build.

## 0. Status

**Proposal; nothing here is built.** §6 names the slice intended first. The rendering
defect in §5 is a live bug independent of everything else and is not part of this proposal.

## 1. Two questions, not one

- **Upgradability** — can an instance that is running the old version continue under the
  new one? A question about rows this deployment already owns.
- **Contracts** — does the process still honour what the outside world was written
  against? A question about parties that are not in the deployment.

They are independent, and folding them costs accuracy in both directions. Two fixtures
report the wrong thing today, and both are the same miscategorisation:

- `shapes/nullable-input-added.yaml` reads **upgradable**, though a caller that omits the
  new property is now rejected at creation. Right about rows, silent about callers.
- `shapes/required-added-to-a-defaulted-property.yaml` reads **breaking**, though every
  stored input carries the key — creation conformed it once and persisted the filled
  default. Right about callers, wrong about rows, and pinned as a false alarm for exactly
  this reason.

Split, both become accurate without touching `isSubset` or the absent-as-null relation.

**Children are deliberately not a third category.** A bundle is checked against itself by
the registration preflight, so upgradability stays a per-process question. A child's own
`output` is still a contract — its consumers include parents outside the bundle.

## 2. Who submits the value sets the direction

| slot | submitter | relation | the conform behind it |
|---|---|---|---|
| process `input` | caller | old ⊆ new | `ValidateInput` at creation |
| process `output` | us | new ⊆ old | a waiting parent's `result_schema` at collect |
| `fetch.result_schema` | the service | old ⊆ new | collect |
| `external.result_schema` | the worker | old ⊆ new | submit |

**A verdict only where a conform stands between the two parties.** Everything else is a
changed slot: a `fetch` request (`url`, `method`, `headers`, `body`) is something we send
into a service whose tolerance is unknowable, and making it a verdict would turn every URL
edit breaking — which `shapes/url-changed.yaml` exists to refuse. `external.input` is the
same case (we send it; the worker's tolerance is its own business).

Two of these are new comparison surface: a narrowing `fetch.result_schema` means **our own**
conform starts rejecting responses it used to accept, and today that shows only as a
changed slot.

## 3. The input is in both categories, under different rules

The same schema pair is asked two different questions, and this is the case that motivates
the whole split:

- **Upgradability** reads the stored input and never conforms it again, so it uses
  `IsSubsetAbsentAsNull` — absence and null navigate identically.
- **Contracts** is what `ValidateInput` will do to the next caller's request, so it is
  strict.

A property that gains `required` while carrying a default is then upgradable (the row has
the value) and contract-breaking (the caller must now send it). Both true, and neither
statement is available today.

## 4. Selection

Default: **everything gates**. Two spellings for the gating set, and they do not combine —
passing both is a usage error, not an intersection:

    genctl compat --from a --to b --check upgrade    # only these gate
    genctl compat --from a --to b --ignore output    # everything but these

**Selection changes the exit code and the emphasis, never what is computed or printed.**
A non-gating finding still appears in the report, marked, and a trailing line names what was
excluded and why the exit is 0. This is the rule `internal/validation/CLAUDE.md` already
states for `nothing_to_compare` — an answer indistinguishable from "checked, and fine" is
worse than no answer — and a selection flag is exactly the feature that invites breaking it.

The two spellings fail in opposite directions: when a contract member is added later,
`--check` silently stops gating on it (fail-open) while `--ignore` gates it by default
(fail-safe). Both are offered anyway, and the not-gating line is what makes the inclusive
one defensible: the new member appears there rather than only in behaviour.

### 4a. Granularity: names and printed paths (intended first)

A token is matched by **exact equality against one attribute of a structured issue** — its
category (`upgrade`, `contract`, `input`, `output`, `fetch`, `external`), its task id, its
leaf field name, or its whole rendered path. Nothing is parsed, so a task called `charge-eu`
or `retry after` needs no syntax of its own, and a token copied out of the report matches
what was printed.

Two consequences, both accepted:

- **No hierarchy.** `--ignore charge` covers a task; `--ignore 'outputs["charge"].fee'`
  covers one field; there is nothing in between and no prefix relation.
- **A rendering change invalidates ignore lists.** The matched-nothing report (below) is
  what surfaces it.

**An exclusion that matched nothing is reported.** It is the only thing that catches a typo
in a free-form segment, and it is what keeps a list from rotting: a break accepted six
months ago and long gone should stop being carried.

A token that collides with a category (a task named `input`) matches both. The not-gating
line names what was actually excluded, so the collision is visible rather than silent.

### 4b. Hierarchical tokens (deferred)

The full form is expression-accessor syntax, which the codebase already commits to for
guard keys ([internal/schema/path.go](../internal/schema/path.go) — injective rendering is
the point):

    --ignore contract.fetch["charge-eu"].result.fee
    --ignore upgrade.outputs["my-task"]

Deferred because it needs step-wise matching to be correct — a string prefix makes
`outputs.a` swallow `outputs.ab`, and a hand-typed `outputs["a"]` must normalize onto the
printed `outputs.a` — which means exporting `parsePath` from `internal/schema`. Also
deferred with it: **wildcards**, **per-process scoping** (`order_proc.contract.output`,
where the process joins the same grammar rather than inventing a `:` separator, process
names being unrestricted), and **`--ignore-file`** reading a policy document, which is what
a long-lived exclusion list actually wants to be.

### 4c. Rejected

- **A second command.** The two verdicts share the expensive half (one `analyze` per side)
  and the fiddly half (`--from`/`--to` resolution, channel-vs-pin selectors, the dependency
  closure, the union-of-names rule). Duplicating that to answer half a question, for an
  operator who is deciding one thing — can I deploy this — is worse on both counts.
- **A boolean per slot.** `--allow-breaking-output` is the first of them and generalises
  into `--ignore output`; a second would have made three spellings of one idea.

## 5. Structured issues (prerequisite)

`genctl` reconstructs a finding by peeling the path off a reason string at the first space
(`splitReason`). Gating cannot key off that, and neither can correct rendering: a
bracket-quoted key may contain a space. The report must carry
`{category, task, slot, path, message}` and the CLI must stop parsing prose.

**Independent of this proposal**, the path in that message is built by a local `joinPath` in
`compat.go` that only concatenates with dots, while `schema.JoinPath` exists and
bracket-quotes. Task ids are unrestricted (only `end` and `next` are reserved), so a task
called `charge-eu` already prints `outputs.charge-eu.fee` — a subtraction to the expression
language — and a task called `a.b` prints something ambiguous with task `a`'s property `b`.
Both are the failure modes `path.go`'s own comment names. That is a bug fix with a fixture,
not a feature.

## 6. What lands first

1. **The rendering fix** (§5, second paragraph) with a weird-name fixture. Independent.
2. **Categories**: two verdict columns, structured issues, the strict input check beside
   the relaxed read, `--check`/`--ignore` at §4a granularity, the not-gating line. No new
   comparison surface — the existing checks are re-partitioned, and the fixtures whose
   verdicts move (§1) move because they were wrong.
3. **Action contracts**: `fetch.result_schema` and `external.result_schema` (§2), paired by
   task id. New surface, so it lands behind settled categories.

## 7. Open

- Does an `external.input` change deserve a verdict after all? The worker is usually code
  the same operator owns, which is an argument the fetch case cannot make.
- Should the roll-up in `SetReport.Compatible` survive? Once gating is a client-side policy,
  a server-side conjunction over everything is one more thing that can disagree with the
  exit code.
- Whether §4a's exact-path tokens are worth shipping at all, or whether names-only is the
  honest stopping point until §4b.
