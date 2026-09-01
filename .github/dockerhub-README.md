# genroc

A durable process orchestrator. Describe a process as tasks in YAML; genroc runs each instance to
completion — surviving crashes, restarts and long waits without holding a thread or losing state.

> **Prototype.** Anything may change between versions. `:preview` is the moving tag; `:latest`
> does not exist yet and will mean something when it does.

```sh
docker run -p 8448:8448 -v genroc:/data genroc/genroc:preview -db /data/genroc.db
```

Then open http://localhost:8448 — this image serves the web UI on the same origin as the API.
Against PostgreSQL, same image: `-pg postgres://user:pass@host/genroc`.

A full stack — engine, UI, script worker, example processes, no credentials:

```sh
curl -O https://raw.githubusercontent.com/genroc/genroc/main/examples/quickstart/compose.yaml
docker compose up
```

## Tags

| tag | what |
|---|---|
| `preview` | newest prerelease — the one to try |
| `0.1.0-rc.1` | pinned and reproducible |
| `edge` | every commit on main |

## Notes

* **Authentication is off by default** and genroc warns loudly. `PUT /definitions` stores code
  the engine runs, so an open port is remote code execution — use `-auth token`.
* **Omit `-ui`** to run headless. Same binary.
* 39.5 MB on disk, ~10 MB compressed.

## The worker

Script tasks need `genroc/eval-node`, which claims them off genroc's queue and evaluates each
function body in its own realm. It needs only `GENROC_SERVER` — and `GENROC_TOKEN` once auth is
on. See the quickstart compose above.

The CLI installs separately: `curl -fsSL https://genroc.org/install.sh | sh`

Source, docs and examples: https://github.com/genroc/genroc
