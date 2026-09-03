# syntax=docker/dockerfile:1
#
# genroc-ui: the web UI and the OIDC login that gets a person into it. specs/ui-component.md.
#
# Build from the REPO ROOT -- ui/ is a module of the workspace, and the context is rooted there:
#
#     docker build -f ui/Dockerfile -t genroc-ui .

FROM --platform=$BUILDPLATFORM node:24-alpine AS assets
WORKDIR /spa
COPY ui/frontend/package*.json ./
RUN npm ci
COPY ui/frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
ARG VERSION=dev
ARG COMMIT=
WORKDIR /src
# GOWORK=off is the point of the split, not a detail: it resolves ui/go.mod ALONE, so the
# server's twenty modules are not merely unused here, they are never downloaded. The whole graph
# is golang-jwt plus a path replace. Building inside the workspace would pull the lot.
ENV GOWORK=off
# Manifests first, so a change to source code does not re-download anything.
COPY ui/go.mod ui/go.sum ./ui/
RUN cd ui && go mod download
COPY ui/ ./ui/
# The compiled SPA replaces the committed placeholder that keeps `go build` working without a
# JavaScript toolchain. Removed first so a stale file cannot survive into the image.
RUN rm -rf ./ui/web
COPY --from=assets /spa/dist ./ui/web
# No cgo: nothing here touches SQLite, which is half of why this image is a fraction of the
# server's and can be built for any architecture without a cross toolchain.
RUN cd ui && CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/genroc-ui .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/genroc-ui /usr/local/bin/
# The same port the server defaults to: genroc-ui stands where genroc would, and machines talk
# to the server directly on its own.
EXPOSE 8448
USER nonroot
ENTRYPOINT ["/usr/local/bin/genroc-ui"]
