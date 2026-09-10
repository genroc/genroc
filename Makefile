db      ?= genroc.db
http    ?= :8448
tcp     ?=
uds     ?=
poll    ?= 500
genroc_server ?= http://localhost:8448
log     ?= info

# BUILD_FLAGS = CGO_ENABLED=1

.PHONY: install extension run build test test-unit test-int test-stress bench-recursive bench-deep bench-drain bench-drain-big bench-iterate swagger client clean generate docs docs-schema docs-build script-runner

run:
	$(BUILD_FLAGS) go run ./cmd/genroc \
		-db $(db) \
		-http $(http) \
		$(if $(tcp),-tcp $(tcp)) \
		$(if $(uds),-uds $(uds)) \
		-poll $(poll) \
		-log $(log) \
		$(ARGS)

# Two modules, three binaries. genroc-ui builds from ./ui, which is a separate module with
# its own go.mod -- see go.work. It embeds whatever is in ui/web; a bare build gets the committed
# placeholder, and the image build overwrites it with ui/frontend compiled.
build: sqlc
	$(BUILD_FLAGS) go build -tags "sqlite_omit_load_extension" -ldflags="-s -w" -o genroc ./cmd/genroc
	$(BUILD_FLAGS) go build -ldflags="-s -w" -o genctl ./cmd/genctl
	$(BUILD_FLAGS) go build -ldflags="-s -w" -o genroc-ui ./ui

# Replace an installed genctl. `prefix` picks the directory; the default is where a `genctl`
# already on PATH lives, and ~/.local/bin otherwise.
#
# Builds to a TEMPORARY name in the target directory and renames over the old one. Copying onto
# the live file instead writes into the inode running processes have mapped, which invalidates
# its signed pages -- macOS then SIGKILLs every new exec from that path (exit 137), while
# `codesign -v` still passes and the file looks fine. A rename swaps the directory entry, so
# anything still running keeps the old inode and the next exec gets the new one.
#
# The version is stamped the way the release build does, plus `-dirty` when the tree is: a
# binary that cannot say which commit it is makes every bug report start with a guess.
prefix ?= $(shell dirname "$$(command -v genctl 2>/dev/null || echo $$HOME/.local/bin/genctl)")
install:
	@mkdir -p "$(prefix)"
	$(BUILD_FLAGS) go build -ldflags="-s -w \
	  -X main.version=edge \
	  -X main.commit=$$(git rev-parse --short HEAD)$$(git diff --quiet || echo -dirty)" \
	  -o "$(prefix)/genctl.new" ./cmd/genctl
	@mv -f "$(prefix)/genctl.new" "$(prefix)/genctl"
	@echo "installed $$("$(prefix)/genctl" -v) -> $(prefix)/genctl"

# The VS Code extension. It is a launcher for `genctl lsp` and ships separately from the
# binaries, so it is not part of `build`.
#
# `compile` BUNDLES with esbuild rather than emitting a tsc tree: vsce reads npm's node_modules
# layout to collect runtime dependencies and will not learn pnpm's (vscode-vsce#421), so the one
# dependency is inlined into dist/ and vsce is told `--no-dependencies`.
#
# The wasm is the server the extension falls back to where the machine has no genctl. ONE module
# for every platform is what keeps the extension a single universal .vsix; a native binary would
# mean one package per platform. It is a plain `go build` -- genctl has no cgo and no sockets.
#
# editors/vscode/LICENSE is a COPY of the root one, not a link: vsce reads the extension
# directory alone, and without a license file there it warns and then stops on a terminal to ask.
# eval-node/LICENSE is a copy for the same reason -- npm publishes that one.
extension:
	GOOS=wasip1 GOARCH=wasm go build -ldflags="-s -w" -o editors/vscode/bin/genctl.wasm ./cmd/genctl
	pnpm install && pnpm -C editors/vscode run compile && pnpm -C editors/vscode run package

test: test-unit test-int

# `./...` matches the CURRENT module only, so each module is listed. Missing one here means its
# tests silently stop running rather than failing.
test-unit:
	$(BUILD_FLAGS) go test ./... ./ui/...

test-stress:
	$(BUILD_FLAGS) go test ./internal/db/... ./internal/engine/... -run TestStress -v --count=3

swagger:
	$(BUILD_FLAGS) go run ./cmd/genrocspec

schema:
	$(BUILD_FLAGS) go run ./cmd/genrocschema $(ARGS)

client: swagger
	pnpm -C tests run generate

test-int: client
	pnpm -C tests run typecheck && pnpm -C tests test

# Spawn benchmarks: YAML-defined workloads (tests/bench/workloads/), SQLite vs Postgres.
# bench-recursive — full binary tree (wide); measures concurrent throughput ceiling.
# bench-deep      — narrow/tall tree; measures per-spawn depth cost.
# bench-drain     — backlog of many independent processes preloaded into a tick-only
#                   server, then drained on restart; measures steady-state queue throughput.
# bench-drain-big — like bench-drain, but each instance carries a ~16 KiB input echoed to
#                   its output (both externalized); isolates per-instance object-store cost.
# recursive/deep defaults are sized to the same instance count (~8k) so the shapes
# compare directly. Set POSTGRES_DSN to also benchmark Postgres.
bench-recursive: client
	pnpm -C tests run bench-recursive

bench-deep: client
	pnpm -C tests run bench-deep

bench-drain: client
	pnpm -C tests run bench-drain

bench-drain-big: client
	pnpm -C tests run bench-drain-big

# bench-iterate — one process that parks many times; the shape that separates
# --durability=terminal from strict (a per-process flush vs a per-yield one).
bench-iterate: client
	pnpm -C tests run bench-iterate

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

# The script-task evaluator (eval-node/). A script task is an `external` task; this is the
# worker that claims them off the queue. See eval-node/README.md.
script-runner:
	pnpm install && cd eval-node && GENROC_SERVER=$(genroc_server) node worker.ts

# The process-definition JSON Schema, as a static file the site serves at
# genroc.org/process-schema.json — the same bytes GET /public/process-schema.json returns, so a
# `# yaml-language-server: $schema=` comment resolves with no genroc running. Generated,
# never committed: it is a projection of internal/model.
docs-schema:
	$(BUILD_FLAGS) go run ./cmd/genrocspec -o "" -schema docs/public/process-schema.json

# The documentation site (docs/). DOCS_BASE sets the subdirectory an archived
# per-version build is served from; unset means the site root.
docs: docs-schema
	pnpm install && pnpm -C docs run dev

docs-build: docs-schema
	pnpm install && pnpm -C docs run build

clean:
	rm -f genroc genctl genroc-ui $(db)
