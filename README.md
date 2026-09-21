# genroc

A durable process orchestrator. You describe a process as a set of tasks in YAML
(or JSON); genroc runs each instance to completion, surviving worker crashes,
restarts, and long waits without holding a thread or losing state.

## Documentation → **<https://genroc.org>**

Install, the definition language, the CLI, the HTTP API and the guides all live there.
Start with [Getting started](https://genroc.org/guides/getting-started/); benchmarks are
at <https://genroc.org/bench/>.

## Run it locally

Go 1.25+ and a C toolchain (SQLite is cgo):

```sh
make build                                           # ./genroc, ./genctl, ./genroc-ui
make install                                         # put this genctl on PATH

./genroc -db genroc.db                               # SQLite (default)
./genroc -pg postgres://user:pass@localhost/genroc   # PostgreSQL
```

It listens on `:8448` (`-http`, `-tcp`, `-uds` to change), which is where `genctl` looks
by default. In another shell, scaffold a project and run its one process:

```sh
genctl init demo -y       # a `hello` process, nothing to configure
cd demo
genctl apply              # register it with the server
genctl run hello --set who=you
genctl get @last          # the instance it just started
genctl logs @last         # its whole tree's trail
```

Docker — the engine, the UI, a script worker and a few processes already registered — is
[examples/quickstart](examples/quickstart); [examples/](examples/) has auth, a login, and
fuller processes.

## Development

```sh
make build      # build (runs sqlc first)
make test       # Go unit tests + TypeScript integration tests, across all modules
make run        # build and run locally
make install    # put this genctl on PATH
make swagger    # regenerate openapi.json
```

[CLAUDE.md](CLAUDE.md) maps the per-area conventions — the dual-engine SQL rules, the
engine invariants, the module boundaries. Design docs live in [specs/](specs/).

## License

Apache-2.0 — see [LICENSE](LICENSE).
