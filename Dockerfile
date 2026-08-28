# syntax=docker/dockerfile:1

# genroc, built static. CGO_ENABLED=0 compiles cleanly and the binary CANNOT open a SQLite
# database — mattn/go-sqlite3 is a stub without cgo and fails at runtime with "requires cgo to
# work". That is fine here and deliberate: this image is for PostgreSQL deployments. A SQLite
# image needs a cgo build on a glibc or musl base instead.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/genroc ./cmd/genroc \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/genctl ./cmd/genctl

# The tooling image: same binaries, plus a shell. `genroc token` and `genctl` are run by
# humans and by init scripts, which need one. Kept as a separate target so the SERVED image
# below has no shell at all — a distroless server is one fewer thing reachable from an RCE.
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
