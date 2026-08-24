db      ?= genroc.db
http    ?= :8448
tcp     ?=
uds     ?=
poll    ?= 500
genroc_server ?= http://localhost:8448
log     ?= info

# BUILD_FLAGS = CGO_ENABLED=1

.PHONY: run build test test-unit test-int test-stress bench-recursive bench-deep bench-drain bench-drain-big swagger client clean generate docs docs-schema docs-build script-runner

run:
	$(BUILD_FLAGS) go run ./cmd/genroc \
		-db $(db) \
		-http $(http) \
		$(if $(tcp),-tcp $(tcp)) \
		$(if $(uds),-uds $(uds)) \
		-poll $(poll) \
		-log $(log) \
		$(ARGS)

build: sqlc
	$(BUILD_FLAGS) go build -tags "sqlite_omit_load_extension" -ldflags="-s -w" -o genroc ./cmd/genroc
	$(BUILD_FLAGS) go build -ldflags="-s -w" -o genctl ./cmd/genctl

test: test-unit test-int

test-unit:
	$(BUILD_FLAGS) go test ./...

test-stress:
	$(BUILD_FLAGS) go test ./internal/db/... ./internal/engine/... -run TestStress -v --count=3

swagger:
	$(BUILD_FLAGS) go run ./cmd/genrocspec

schema:
	$(BUILD_FLAGS) go run ./cmd/genrocschema $(ARGS)

client: swagger
	cd tests && npm run generate

test-int: client
	cd tests && npm run typecheck && npm test

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
	cd tests && npm run bench-recursive

bench-deep: client
	cd tests && npm run bench-deep

bench-drain: client
	cd tests && npm run bench-drain

bench-drain-big: client
	cd tests && npm run bench-drain-big

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

# The script-task evaluator (evaluator/). A script task is an `external` task; this is the
# worker that claims them off the queue. See evaluator/README.md.
script-runner:
	cd evaluator && npm install && GENROC_SERVER=$(genroc_server) node worker.ts

# The process-definition JSON Schema, as a static file the site serves at
# genroc.org/process-schema.json — the same bytes GET /process-schema.json returns, so a
# `# yaml-language-server: $schema=` comment resolves with no genroc running. Generated,
# never committed: it is a projection of internal/model.
docs-schema:
	$(BUILD_FLAGS) go run ./cmd/genrocspec -o "" -schema docs/public/process-schema.json

# The documentation site (docs/). DOCS_BASE sets the subdirectory an archived
# per-version build is served from; unset means the site root.
docs: docs-schema
	cd docs && npm install && npm run dev

docs-build: docs-schema
	cd docs && npm install && npm run build

clean:
	rm -f genroc genctl $(db)
