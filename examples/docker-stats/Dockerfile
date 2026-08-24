# Copyright (c) 2026 TRV Enterprises LLC
# SPDX-License-Identifier: Apache-2.0
#
# Containerized docker-stats collector.
#
# Built primarily for Synology DSM, where the native-binary deployment used on
# ordinary Linux hosts does not work:
#
#   - /var/run/docker.sock is root:root 0660 and DSM has NO `docker` group, so
#     the SupplementaryGroups=docker pattern the systemd unit relies on has
#     nothing to attach to. A non-root service simply cannot read the socket.
#   - DSM has no systemd convention to hook, and wipes unmanaged files on
#     upgrade, so a binary dropped on the filesystem is not durable.
#
# Running as a container solves both: the process is root inside its own
# namespace (no root grant on the host), Container Manager supervises restarts,
# and the deployment is a compose file that survives DSM upgrades. It is also
# the most portable thing to hand someone else — no install script.
#
# BUILD CONTEXT IS THE REPO ROOT, not this directory: docker-stats is part of
# the root Go module and needs go.mod/go.sum from there.
#
#   docker build -f examples/docker-stats/Dockerfile -t docker-stats .
#
# The socket is mounted READ-ONLY. The collector only ever GETs from the Docker
# API (/containers/json, /containers/{id}/stats, /info, /system/df) — it never
# creates, starts, or stops anything. Note this is still a privileged mount:
# read access to the Docker socket discloses everything about every container
# on the host, so treat the container as trusted even though it cannot mutate.

# Keep this in step with go.mod's `toolchain` directive. The builder image
# ships GOTOOLCHAIN=local, so it will NOT download a newer toolchain — a
# stale pin here silently builds the image on the old Go while CI (which
# resolves from go.mod) uses the new one. That divergence is invisible: the
# build succeeds and only the binary's stamped version reveals it.
FROM golang:1.27-alpine AS builder

WORKDIR /app

# Module files first so the dependency layer caches independently of source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-w -s -X main.Version=${VERSION}" \
      -o docker-stats ./examples/docker-stats

FROM alpine:3.19

# ca-certificates for HTTPS to a remote ts-store; tzdata so log timestamps
# render in the host's zone rather than UTC-only.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /app/docker-stats /usr/local/bin/docker-stats

# Deliberately NOT dropped to a non-root user. The whole reason this image
# exists is that the Docker socket is root-owned with no group to join —
# a non-root UID inside the container would hit the same permission wall.
# Containment comes from the namespace and the read-only mount, not from UID.
USER root

ENTRYPOINT ["/usr/local/bin/docker-stats"]
