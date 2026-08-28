# Authentication demo

genroc with `--auth token`, credentials scoped to what each caller needs, and a script worker
that holds the weakest one. specs/api-auth.md.

    ./examples/auth/gen-env.sh                       # once — mints both credentials
    docker compose -f examples/auth/compose.yaml up --build

Both credentials are written to `examples/auth/.env` and neither is printed — echoing a secret
that is already in a file only adds a copy to your scrollback.

`OPS_TOKEN` is your admin credential. Store it wherever you keep secrets, then delete the line:
genroc has its hash after the first start and never needs the value again. `WORKER_TOKEN` stays,
because the evaluator reads it at every start.

You drive the stack from the host, so no container ever holds an admin credential:

    GENROC_TOKEN=… genctl token list
    GENROC_TOKEN=… genctl definitions

## What it shows

**Two credentials, scoped.** `init` mints an `admin` token for the operator and a `worker`
token for the evaluator, and nothing holds more than it needs. Measured against the running
stack:

| as the worker token | |
|---|---|
| `POST /api/external-tasks/claim` | 200 |
| `GET /api/definitions` | 403 |
| `GET /api/instances` | 403 |
| `GET /api/tokens` | 403 |

That is the point of the split: a leaked worker credential — the one that sits on whatever
machine runs untrusted scripts — cannot read a definition, list a run, or mint another token.

**Two credentials, two lifetimes.** The worker token stays in `.env` because a container reads
it at every start with no human present. The admin token is there for the FIRST start only —
genroc keeps its hash, so later starts do not need the secret, and deleting the line stops it
resting in a file compose reads and in an environment `docker inspect` can print. Verified:

    first start                     created=2
    (delete OPS_TOKEN, restart)     created=0  no_secret_supplied=ops
    the stored token                still works

`no_secret_supplied` names the label deliberately, so a credential that vanished by accident
looks different from one you removed on purpose.

**The operator supplies the credentials; genroc never mints one.** `gen-env.sh` runs
`genctl token generate` — offline, no server, no credential — and writes a gitignored `.env`
that compose reads. genroc receives them through `GENROC_SEED_TOKENS` and stores only their
hashes, so **a secret never originates inside genroc, never reaches its logs, and never rests
in its container**. Verified on the running stack: `grep genroc_sk_` over genroc's log returns
nothing, and it reports `seeded operator-supplied credentials created=2`.

Seeding is idempotent by secret, so a restart is a no-op (`created=0`) and rotation is
additive — the old credential keeps working until it is revoked, which is what lets a fleet
roll without a window where half the workers are refused.

`genroc` still auto-mints a bootstrap admin if you start it with neither — that path exists for
`docker run` and prints a rotate warning, because the secret then goes to a log.

**`/healthz` stays open.** Unauthenticated from the host it answers 200 while `/api/*` answers
401 — a probe must answer before any identity exists, so it is the one route outside both the
API prefix and the gate (§1).

**A worker outlives the server it polls.** The evaluator starts alongside genroc and loses the
first few claims to `ECONNREFUSED`; a network error is a reply rather than a throw, so it keeps
polling and picks up work the moment the server is listening. That matters beyond this demo —
a rolling deploy or a server restart must not kill a fleet of workers.

**The served image has no shell.** The `server` stage is distroless; `sh` exists only in the
`tools` stage that `init` and `ops` use. `docker compose exec genroc sh` fails, which is the
intent.

## What it does NOT show

**SSO for humans.** `jwt` and `header` modes are not built, so there is no path for a browser
behind an identity provider — machines are served, people are not. When they land, the shape is
oauth2-proxy in front of `/` with the API left on `/api/*` (§5.1), and this file grows a proxy
service.

**TLS.** Everything here is plain HTTP on a compose network. A bearer token in cleartext is as
exposed as a password, so a real deployment terminates TLS at an ingress or a proxy (§9).

## Notes

- The images use **PostgreSQL**. The Dockerfile builds with `CGO_ENABLED=0`, which cannot open
  a SQLite database — `mattn/go-sqlite3` is a stub without cgo. A SQLite image needs a cgo
  build on a glibc or musl base.
- **`down` keeps the database; `down -v` resets it.** That distinction matters here: the admin
  row lives in Postgres, so dropping the volume takes your stored credential with it — and
  genroc, finding no admin, mints a fresh one and prints it to the log. After a `down -v`,
  re-run `gen-env.sh`.
- For a one-shot command against the database — minting a replacement admin after a reset, say
  — there is a `tools` profile that needs no running server and no credential:

      docker compose -f examples/auth/compose.yaml run --rm tools \
        token create -pg "$PG" --perms admin --label ops

- `examples/auth/.env` is gitignored and written `0600`. Delete it and re-run `gen-env.sh` to
  rotate; the old tokens keep working until revoked, so revoke them after the fleet has rolled.
- The credentials do reach `docker inspect`, being environment variables. That is the usual
  compose trade-off; a real deployment uses its platform's secret mechanism, which is the same
  `GENROC_SEED_TOKENS` value delivered differently.
