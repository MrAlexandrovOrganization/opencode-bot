# syntax=docker/dockerfile:1

FROM golang:1.26.6-alpine AS builder

# Pinned via build args from the Makefile (single source of truth).
ARG PROTOC_GEN_GO_VERSION
ARG PROTOC_GEN_GRPC_VERSION

# BuildKit cache mounts keep the module and build caches across image layers,
# so `go mod download` and `go build` do not start from scratch. The caches are
# owned by the builder, not by the host, so the same Dockerfile works on Linux
# and on Docker Desktop (macOS) — the first build there is simply cold.
RUN --mount=type=cache,id=opencode-bot-go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=opencode-bot-go-build,target=/root/.cache/go-build \
    apk add --no-cache protobuf-dev && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GRPC_VERSION}

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,id=opencode-bot-go-mod,target=/go/pkg/mod \
    go mod download

COPY proto/ /proto/
COPY . .

RUN --mount=type=cache,id=opencode-bot-go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=opencode-bot-go-build,target=/root/.cache/go-build \
    mkdir -p gen/whisper && \
    protoc -I /proto \
        --go_out=gen/whisper --go_opt=paths=source_relative \
        --go-grpc_out=gen/whisper --go-grpc_opt=paths=source_relative \
        /proto/whisper.proto

RUN --mount=type=cache,id=opencode-bot-go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=opencode-bot-go-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /bot ./cmd/bot

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata ffmpeg
COPY --from=builder /bot /bot
CMD ["/bot"]
