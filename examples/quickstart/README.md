# genroc, the short way in

    docker compose -f examples/quickstart/compose.yaml up --build
    open http://localhost:8448

No credentials, no setup script, nothing to build first. You get the engine, the UI, a script
worker, and three example processes already registered so the screen is not empty.

    NAME              VERSION  RAISES
    expense-approval  v1       expense_rejected
    hello-script      v1
    script            v1       script_threw, script_timeout, script_unknown

Run one from the UI, or:

    export GENROC_SERVER=http://localhost:8448
    genctl run hello-script --set name=you
    genctl instances

## There is no authentication here, and genroc says so

    WARN  API is UNAUTHENTICATED and bound beyond loopback — anyone who reaches this port
          can register a definition, which is arbitrary code execution on this server.

That is not a caveat in small print: `PUT /definitions` stores code the engine will run, so an
open port is remote code execution. It is the right trade on a laptop and the wrong one the
moment anyone else can reach the port.

The UI says the same thing in its header rather than showing a credential field you do not need.

## When that stops being right

genroc is the same binary in all three of these, so nothing you do here is thrown away — only
the flags change.

| | adds | for |
|---|---|---|
| this example | — | a laptop |
| [examples/auth](../auth) | `-auth token` | machines: CI, workers, scripts |
| [examples/proxy](../proxy) | SSO through a proxy | people, with real login |

The ladder is deliberate. Machine tokens come first because they are what a worker needs, and a
worker is the first thing that outlives your terminal.

## What is in the image

`genroc/platform` — the engine and the UI, served from one origin, so `/api` is same-origin from
the browser and there is no CORS anywhere in the system. `genroc/server` is the same engine with
no UI, for a cluster that fronts it with something else.

The UI is built by the Dockerfile, not committed and not embedded in the binary: `go build` stays
free of node, and there is no committed placeholder that can silently ship a broken UI when
someone forgets to run npm.

## Postgres, not SQLite

The image is built `CGO_ENABLED=0`, and `mattn/go-sqlite3` is a stub without cgo — a SQLite
database fails at runtime with *"requires cgo to work"*. So the compose file runs Postgres, which
is what a deployment uses anyway. `./genroc -db genroc.db` on your machine still works; that
binary is built with cgo.
