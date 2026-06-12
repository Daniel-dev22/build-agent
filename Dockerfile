# syntax=docker/dockerfile:1.7
# Build-agent: orchestrates `docker buildx` + `git` on the NAS and reports build
# jobs to controller. The runtime needs the docker CLI + buildx plugin + git
# (it shells out to them against the host docker socket).
#
# NOTE: this image is BOOTSTRAPPED by the Python orchestrator, which fetches the
# private agent-kit-go module over SSH (--mount=type=ssh). Once the build-agent
# is running it builds every OTHER repo with a GitHub App installation token
# (--mount=type=secret=ghtoken, see engine.go) — the SSH path here is only the
# self-bootstrap. Phase 3 converts this to the token path too.
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
RUN apk add --no-cache docker-cli docker-cli-buildx git ca-certificates
COPY --from=builder /out/build-agent /usr/local/bin/build-agent
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/health/ready || exit 1
ENTRYPOINT ["build-agent"]
