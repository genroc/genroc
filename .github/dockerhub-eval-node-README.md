# genroc/eval-node

The script-task worker for [genroc](https://genroc.org). It claims TypeScript and JavaScript
script tasks from a genroc server's queue and runs each one in a fresh worker thread.

> **Prototype.** Anything may change between versions; pin a version tag for anything you keep.

```sh
docker run -e GENROC_SERVER=http://genroc:8448 genroc/eval-node:latest
```

The worker connects to genroc, not the other way round, so it needs only outbound access and
no published ports.

This image only runs scripts. Bundling and typechecking happen on your machine, through the
[`@genroc/eval-node`](https://www.npmjs.com/package/@genroc/eval-node) npm package that
`genctl apply` calls. To scaffold a project with both, run `genctl init --eval-node`.

## Compose

```yaml
services:
  eval-node:
    image: genroc/eval-node:latest
    environment:
      GENROC_SERVER: http://genroc:8448
    restart: unless-stopped
```

To run more scripts at once, scale the service or raise `CONCURRENCY`.

## Environment

| env | default | |
|---|---|---|
| `GENROC_SERVER` | `http://localhost:8448` | the genroc server |
| `GENROC_TOKEN_FILE` | *(none)* | file holding the token, when the server uses `--auth token` |
| `GENROC_TOKEN` | *(none)* | the token inline; wins over the file |
| `PROCESS` / `TASK` | `script-node` / `eval_node` | claim only this process / task id; the defaults are script-node's. Set `PROCESS` for a renamed copy, or `""` to drop a filter; **with both `""`, the worker claims every external task.** |
| `CONCURRENCY` | `4` | scripts run at once |
| `LEASE_MS` | `30000` | claim lease, renewed every third of it |
| `POLL_MS` | `250` | idle poll interval |
| `WORKER_ID` | `evaluator-<pid>` | the claim holder's name |

With auth enabled, create a token that has only the `worker` permission, and mount it as a file
so it stays out of `docker inspect`:

```sh
genctl token create --perms worker --label evaluator -q > worker-token
docker run -e GENROC_SERVER=http://genroc:8448 \
  -e GENROC_TOKEN_FILE=/run/worker-token -v "$PWD/worker-token:/run/worker-token:ro" \
  genroc/eval-node:latest
```

If the token is rejected, the container exits instead of retrying.

## Tags

| tag | what |
|---|---|
| `latest` | newest release |
| `0.20.0`, `0.20`, `0` (for example) | a release, pinned to its version, minor or major |
| `edge` | every commit on main |

A release tags the genroc server, this image and the `@genroc/eval-node` package with the same
version.

## Notes

* **Not a sandbox.** Scripts get the container's filesystem, network and environment. Run
  only code you trust.
* **Memory is not isolated.** A script that runs out of memory takes down the whole worker.
  Run it with `restart: unless-stopped`.
* Also on `ghcr.io/genroc/eval-node`.

Docs: [TypeScript evaluation](https://genroc.org/guides/process-definition/typescript-evaluation/)
· Source: https://github.com/genroc/genroc/tree/main/eval-node
