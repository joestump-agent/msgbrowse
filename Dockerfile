# syntax=docker/dockerfile:1

# --- build stage ---
# msgbrowse uses the pure-Go modernc.org/sqlite driver (FTS5 built in), so the
# build needs no C toolchain — just the Go image.
# The official golang images set GOTOOLCHAIN=local, which would silently ignore
# the go.mod toolchain directive — pin the exact patch so the shipped image gets
# stdlib security fixes (see GO-2026-5856).
FROM golang:1.25.12-bookworm AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pre-create the data dir so the runtime stage can own it (distroless has no
# shell to mkdir/chown at runtime). A fresh named volume mounted over /data
# inherits this ownership, so the non-root user can write the SQLite DB.
RUN mkdir -p /data

ARG VERSION=docker
ARG COMMIT=none
ARG BUILD_DATE=unknown

# The driver is pure Go, so build a fully static binary (no cgo). Strip debug
# info for size.
ENV CGO_ENABLED=0
RUN go build \
      -ldflags "-s -w \
        -X github.com/joestump/msgbrowse/internal/cli.Version=${VERSION} \
        -X github.com/joestump/msgbrowse/internal/cli.Commit=${COMMIT} \
        -X github.com/joestump/msgbrowse/internal/cli.BuildDate=${BUILD_DATE}" \
      -o /out/msgbrowse ./cmd/msgbrowse

# --- runtime stage ---
# distroless/static-debian12 ships CA certs + tzdata and a non-root "nonroot"
# user (uid 65532). No libc, no shell, no package manager — the smallest base
# that a fully static Go binary needs. Minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/msgbrowse /usr/local/bin/msgbrowse

# /data is the writable app-data dir, owned by the non-root user (uid 65532 in
# distroless static). A fresh named volume mounted here is initialized with this
# ownership so the SQLite database can be created.
COPY --from=build --chown=65532:65532 /data /data

# Writable app data lives in /data (a named volume); the archive is mounted
# read-only at /archive by compose. Defaults point the binary at both.
ENV MSGBROWSE_DATA_DIR=/data \
    MSGBROWSE_ARCHIVE_ROOT=/archive \
    MSGBROWSE_LISTEN_ADDR=0.0.0.0:8787

# The server binds inside the container; compose maps it to host loopback only.
EXPOSE 8787

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/msgbrowse"]
CMD ["serve"]
