# genroc, the short way in

> **Prototype.** Anything may change between versions — wire format, CLI, stored data. No
> deprecation window, no upgrade shim. There is no `latest` tag yet: `:preview` is the moving
> one, `0.1.0-rc.1` is what you pin.

    docker compose -f examples/quickstart/compose.yaml up --build
    open http://localhost:8448

Two containers, no credentials, nothing to build first. The engine, the UI, a script worker, and
three processes already registered:

    NAME              VERSION  RAISES
    expense-approval  v1       expense_rejected
    hello-script      v1
    script            v1       script_threw, script_timeout, script_unknown

Run one from the UI, or:

    export GENROC_SERVER=http://localhost:8448
    genctl run hello-script --set name=you
    genctl instances

To use the published image instead of building, swap `build:` for
`image: ghcr.io/genroc/genroc:preview`.

## No authentication, and genroc says so

    WARN  API is UNAUTHENTICATED and bound beyond loopback — anyone who reaches this port
          can register a definition, which is arbitrary code execution on this server.

`PUT /definitions` stores code the engine runs, so an open port is RCE. Fine on a laptop; not
once anyone else can reach it. The UI says the same in its header rather than showing a
credential field you do not need.

## The ladder

Same image throughout — only flags change.

| | adds | for |
|---|---|---|
| this example | — | a laptop |
| [examples/auth](../auth) | `-auth token` | machines: CI, workers, scripts |
| [examples/proxy](../proxy) | SSO through a proxy | people, with real login |

## The image

`ghcr.io/genroc/genroc` — engine, UI and SQLite, 39.5 MB on disk and ~10 MB to pull. One origin
serves API and UI, so there is no CORS anywhere. Omit `-ui` and it runs headless; the UI is
229 kB, which is why there is no second image to choose between.

`ghcr.io/genroc/eval-node` is the script worker — 231 MB, almost all of it Node. It installs no
packages: `worker.ts` imports only node builtins, and the bundler deps belong to the author-time
resolver genctl runs locally.

Both are on Docker Hub too, since that is the only registry `docker run` resolves without a host
prefix.
