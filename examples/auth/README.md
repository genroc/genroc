# Authentication demo

genroc with `--auth token`, credentials scoped to what each caller needs, and a script worker
that holds the weakest one. specs/api-auth.md.

    ./examples/auth/gen-secrets.sh                   # once — mints both credentials
    docker compose -f examples/auth/compose.yaml up --build

Both credentials are written to `examples/auth/secrets/` and neither is printed — echoing a
secret that is already in a file only adds a copy to your scrollback.

The `admin=` line of `secrets/seed-tokens` is your admin credential. Store it wherever you keep
secrets, then delete the line: genroc has its hash after the first start and never needs the value
again. `secrets/worker-token` stays, because the evaluator reads it at every start.

You drive the stack from the host, so no container ever holds an admin credential:

    genctl config set token genroc_sk_…      # ~/.config/genroc/config.yaml, 0600
    genctl token list
    genctl definitions

`config set` rather than `$GENROC_TOKEN`, which every process you start inherits and `ps` can
show. The variable still wins where a file is the wrong shape, which is CI.

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

**Two credentials, two lifetimes.** The worker token stays because a container reads it at
every start with no human present. The admin line is there for the FIRST start only — genroc
keeps its hash, so later starts do not need the secret, and deleting the line stops it resting in
a file compose mounts:

    first start                      created=2
    (delete the admin line, restart)   created=0
    the stored token                 still works

Blanking the secret instead (`admin=admin=`) logs `no_secret_supplied=admin`, so a credential you
removed on purpose reads differently from one that vanished by accident.

**The operator supplies the credentials; genroc never mints one.** `gen-secrets.sh` runs
`genctl token generate` — offline, no server, no credential — and writes gitignored files that
compose mounts as secrets. genroc reads them through `GENROC_SEED_TOKENS_FILE` and stores only
their hashes, so **a secret never originates inside genroc and never reaches its logs**. Each
container gets only its own file: the evaluator never sees the admin line.

Seeding is idempotent by secret, so a restart is a no-op (`created=0`) and rotation is
additive — the old credential keeps working until it is revoked, which is what lets a fleet
roll without a window where half the workers are refused.

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

**SSO for humans.** genroc verifies JWTs from whatever IdP you run, and this example uses none:
everything here is a machine holding a token, which is what keeps an identity provider out of the
stack entirely. `examples/ui/` is the other half — genroc-ui, which authenticates a person and
mints the token genroc verifies.

**TLS.** Everything here is plain HTTP on a compose network. A bearer token in cleartext is as
exposed as a password, so a real deployment terminates TLS at an ingress or a proxy (§9).

## Notes

- The image carries a statically linked cgo build, so **SQLite and PostgreSQL both work**.
  This example uses PostgreSQL because that is what a deployment uses; `--db /data/genroc.db`
  is the same image with one flag changed.

- **`down` keeps the database; `down -v` resets it.** That distinction matters here: the admin
  row lives in Postgres, so dropping the volume takes your stored credential with it, and genroc
  starts with a `no admin token` warning. After a `down -v`, delete `secrets/` and re-run
  `gen-secrets.sh`.
- For a one-shot command against the database — minting a replacement admin after a reset, say
  — there is a `tools` profile that needs no running server and no credential:

      docker compose -f examples/auth/compose.yaml run --rm tools \
        token create --pg "$PG" --perms admin --label admin

- `examples/auth/secrets/` is gitignored and its files are written `0644`, so the containers'
  non-root users can read the mounts — a development default. Delete it and re-run
  `gen-secrets.sh` to rotate; the old tokens keep working until revoked, so revoke them after the
  fleet has rolled.
- A real deployment uses its platform's secret mechanism the same way: a file, named by
  `GENROC_SEED_TOKENS_FILE`.
