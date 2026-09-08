# syntax=docker/dockerfile:1.7
# build-agent image.
#
# Stage 1 (builder) compiles the static, CGO-free binary, cross-compiling to
# ${TARGETARCH} so multi-arch builds never run the Go toolchain under emulation.
# Stage 2 is the runtime: it needs the docker CLI + buildx plugin + git, because
# the agent shells out to them against the host's mounted docker socket. It also
# needs make, because a descriptor's `pre_build` hooks stamp generated sources by
# invoking the upstream project's own build rules (frigate's `make version`)
# rather than reimplementing them here.
#
# The builder fetches github.com/Daniel-dev22/agent-kit-go, which is still a
# PRIVATE module — hence GOPRIVATE, the SSH url rewrite, and `--mount=type=ssh`
# on `go mod download` (build with `docker buildx build --ssh default`). No key
# is ever copied into a layer. `go build ./...` on the host needs none of this
# once the module is in your module cache; see the README. When agent-kit-go is
# published these four lines can be deleted.
#
# Unrelated to the above: at RUNTIME the agent builds OTHER private repos using a
# GitHub App installation token vended by the controller and passed to buildx as
# `--secret id=ghtoken` (see engine.go) — not over SSH.
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git openssh-client ca-certificates
WORKDIR /app

ENV GOPRIVATE=github.com/Daniel-dev22/*
RUN git config --global url."git@github.com:".insteadOf "https://github.com/" && \
    mkdir -p /root/.ssh && ssh-keyscan github.com >> /root/.ssh/known_hosts 2>/dev/null

COPY go.mod go.sum* ./
RUN --mount=type=ssh go mod download

COPY *.go ./
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-w -s" -o /out/build-agent .

FROM alpine:3.21
RUN apk add --no-cache docker-cli docker-cli-buildx git make ca-certificates
COPY --from=builder /out/build-agent /usr/local/bin/build-agent
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/health/ready || exit 1
ENTRYPOINT ["build-agent"]
