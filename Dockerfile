# syntax=docker/dockerfile:1
#
# One image: genroc/genroc — engine, UI and SQLite. Omit `-ui` to run it headless.

FROM --platform=$BUILDPLATFORM node:24-alpine AS ui
WORKDIR /ui
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Not cross-compiled: the SQLite driver is C, so genroc needs cgo, and cgo cannot target another
# architecture without a toolchain. CI builds each arch on its own runner (release.yml).
# `-extldflags -static` links musl in so the result still runs on distroless/static.
FROM golang:1.25-alpine AS build
ARG VERSION=dev
ARG COMMIT=
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w -extldflags '-static' -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/genroc ./cmd/genroc \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/genctl ./cmd/genctl \
 && mkdir -p /empty

FROM gcr.io/distroless/static-debian12:nonroot AS genroc
# genctl rides along so an init container or `docker exec` needs no second image.
COPY --from=build /out/genroc /out/genctl /usr/local/bin/
COPY --from=ui /ui/dist /srv/ui
# Docker seeds a fresh named volume from this directory INCLUDING ownership. Without it the
# volume is root-owned and SQLite fails as "unable to open database file: no such file or
# directory" — an errno that points at the wrong cause. Bind mounts keep host ownership: chown
# them to 65532.
COPY --from=build --chown=nonroot:nonroot /empty /data
# Safe because of -auth, not the bind address; `-auth none` warns on startup.
EXPOSE 8448
USER nonroot
ENTRYPOINT ["/usr/local/bin/genroc"]
CMD ["-ui", "/srv/ui"]
