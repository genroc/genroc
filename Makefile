db      ?= genroc.db
http    ?= :8448
tcp     ?=
uds     ?=
poll    ?= 500
log     ?= info

# BUILD_FLAGS = CGO_ENABLED=1

.PHONY: run build test test-unit test-int test-stress bench-recursive bench-deep bench-drain bench-drain-big swagger client clean generate docs docs-build

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
	cd tests && bun run generate

test-int: client
	cd tests && ~/.bun/bin/bun run typecheck && ~/.bun/bin/bun run test

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
	cd tests && ~/.bun/bin/bun run bench-recursive

bench-deep: client
	cd tests && ~/.bun/bin/bun run bench-deep

bench-drain: client
	cd tests && ~/.bun/bin/bun run bench-drain

bench-drain-big: client
	cd tests && ~/.bun/bin/bun run bench-drain-big

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

# The documentation site (docs/). DOCS_BASE sets the subdirectory an archived
# per-version build is served from; unset means the site root.
docs:
	cd docs && ~/.bun/bin/bun install && ~/.bun/bin/bun run dev

docs-build:
	cd docs && ~/.bun/bin/bun install && ~/.bun/bin/bun run build

clean:
	rm -f genroc genctl $(db)
