DOCKER_COMPOSE = docker compose

GO_UNIT_PKGS = \
	./cmd/... \
	./internal/... \
	./internal/backend/... \
	./internal/bot/... \
	./internal/config/... \
	./internal/whisper/...

# Canonical source of whisper.proto.
# For remote fetch (e.g. in CI without access to the backend repo):
#   make proto WHISPER_PROTO_SRC=https://raw.githubusercontent.com/org/transcriber/main/proto/whisper.proto
WHISPER_PROTO_SRC ?= ../../backends/transcriber/proto/whisper.proto

# Export canonical versions to both Dockerfiles through Compose.
include versions.mk

# Install all dev tools: Go protoc plugins.
# Requires protoc on PATH (e.g. apt install protobuf-compiler).
.PHONY: install
install:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)

# Build and (re)start everything, including the heavy opencode-server image.
.PHONY: up
up:
	$(DOCKER_COMPOSE) up -d --build

# Fast loop for bot-only changes: never touches opencode-server.
.PHONY: dev
dev:
	$(DOCKER_COMPOSE) up -d --build --no-deps opencode-bot

.PHONY: down
down:
	$(DOCKER_COMPOSE) down

.PHONY: logs
logs:
	$(DOCKER_COMPOSE) logs -f

.PHONY: restart
restart:
	$(DOCKER_COMPOSE) restart

.PHONY: format
format:
	gofmt -w ./cmd ./internal

.PHONY: fmt-check lint check
fmt-check:
	@files=$$(gofmt -l ./cmd ./internal) || { echo "gofmt failed"; exit 1; }; \
	if [ -n "$$files" ]; then echo "Files need formatting:"; printf '%s\n' "$$files"; exit 1; fi

lint:
	go vet ./cmd/... ./internal/...

.PHONY: test
test:
	go test $(GO_UNIT_PKGS)

check: fmt-check lint test config-check

.PHONY: cover
cover:
	go test -coverprofile=/tmp/cover.out ./... && go tool cover -func=/tmp/cover.out

# Sync proto from the canonical source (backends/transcriber), patch the
# go_package to this module and regenerate Go stubs.
# Requires: protoc + protoc-gen-go + protoc-gen-go-grpc  →  make install
.PHONY: proto
proto:
	@echo "Syncing proto from $(WHISPER_PROTO_SRC)..."
	@if echo "$(WHISPER_PROTO_SRC)" | grep -qE "^https?://"; then \
		curl -sSfL "$(WHISPER_PROTO_SRC)" -o proto/whisper.proto; \
	else \
		cp "$(WHISPER_PROTO_SRC)" proto/whisper.proto; \
	fi
	sed -i 's|go_package = .*|go_package = "opencode-bot/gen/whisper";|' proto/whisper.proto
	mkdir -p gen/whisper
	protoc -I proto \
		--go_out=gen/whisper --go_opt=paths=source_relative \
		--go-grpc_out=gen/whisper --go-grpc_opt=paths=source_relative \
		proto/whisper.proto

# Build and start the opencode server service (Docker).
# Workspace: ~/projects (rw), rest of home: read-only.
# Survives reboots via restart: unless-stopped.
.PHONY: server
server:
	$(DOCKER_COMPOSE) up -d --build opencode-server

.PHONY: versions compose-build
versions:
	@$(foreach v,$(VERSION_VARS),printf '%s=%s\n' '$(v)' '$($(v))';)
compose-build:
	$(DOCKER_COMPOSE) build

.PHONY: config-check
config-check:
	@set -eu; \
	created_env=0; \
	if test ! -e .env && test ! -L .env; then \
		(umask 077; set -C; : > .env) || { echo '.env appeared while preparing config check' >&2; exit 1; }; \
		created_env=1; \
	fi; \
	trap 'if test "$$created_env" = 1; then rm -f .env; fi' EXIT; \
	$(DOCKER_COMPOSE) --env-file /dev/null config --quiet --no-env-resolution

# CI builds/publishes once; VM activates immutable digests (Python 3 + Docker required).
export IMAGE_COMPOSE_FILE := docker-compose.yml
.PHONY: image-build image-publish image-deploy image-activate
image-build:
	python3 scripts/images.py build
image-publish:
	python3 scripts/images.py publish
image-deploy:
	python3 scripts/images.py deploy
image-activate:
	$(IMAGE_COMPOSE) up -d --no-build --pull never --wait --wait-timeout 180

.PHONY: image-check
image-check:
	python3 -B scripts/test_images.py

.PHONY: image-compose
export IMAGE_ARGS
image-compose:
	python3 scripts/images.py compose

include makefiles/ci-deploy.mk
