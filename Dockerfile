# syntax=docker/dockerfile:1
#
# Two shipped images, from one build:
#
#   genroc/server     the engine alone. What runs in a cluster.
#   genroc/platform   engine + UI, or UI pointed at a remote engine. What you run to try genroc,
#                     and what a small deployment keeps running.
#
#   docker build --target server   -t genroc/server   .
#   docker build --target platform -t genroc/platform .

# The UI is built HERE rather than embedded in the binary. Embedding would make `go build`
# depend on node, or on a committed placeholder that silently ships a broken UI when someone
# forgets to run npm. A build stage keeps the Go build free of node and still leaves nothing
# for a user to run before `docker compose up`.
FROM node:24-alpine AS ui
WORKDIR /ui
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# genroc, built static. CGO_ENABLED=0 compiles cleanly and the binary CANNOT open a SQLite
# database — mattn/go-sqlite3 is a stub without cgo and fails at runtime with "requires cgo to
# work". That is fine here and deliberate: these images are for PostgreSQL deployments. A SQLite
# image needs a cgo build on a glibc or musl base instead.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/genroc ./cmd/genroc \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/genctl ./cmd/genctl

# The tooling image: same binaries, plus a shell. `genroc token` and `genctl` are run by humans
# and by init scripts, which need one. Kept separate so the SERVED images below have no shell at
# all — a distroless server is one fewer thing reachable from an RCE.
FROM alpine:3.20 AS tools
COPY --from=build /out/genroc /out/genctl /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/genctl"]

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/genroc /out/genctl /usr/local/bin/
# Bound to every interface because a container publishes a port; what keeps it safe is -auth,
# not the bind address. Started with `-auth none` this logs a warning saying so.
EXPOSE 8448
USER nonroot
ENTRYPOINT ["/usr/local/bin/genroc"]

# The same server with the UI beside it. One origin serves both, so `/api` is same-origin from
# the browser and no CORS exists anywhere in the system.
FROM server AS platform
COPY --from=ui /ui/dist /srv/ui
# A default rather than a requirement: `-ui` still overrides, and the server image ignores this
# path entirely because it has no UI to serve.
CMD ["-ui", "/srv/ui"]
