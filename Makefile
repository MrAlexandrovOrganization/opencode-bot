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

# Install all dev tools: Go protoc plugins.
# Requires protoc on PATH (e.g. apt install protobuf-compiler).
.PHONY: install
install:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

.PHONY: up
up:
	$(DOCKER_COMPOSE) up -d --build

.PHONY: down
down:
	$(DOCKER_COMPOSE) down

.PHONY: logs
logs:
	$(DOCKER_COMPOSE) logs -f

.PHONY: restart
restart:
	$(DOCKER_COMPOSE) restart

.PHONY: deploy
deploy:
	$(DOCKER_COMPOSE) up -d --build --no-cache

.PHONY: format
format:
	gofmt -w ./cmd ./internal

.PHONY: lint
lint:
	@if [ -n "$$(gofmt -l ./cmd ./internal)" ]; then \
		echo "Files need formatting:"; \
		gofmt -l ./cmd ./internal; \
		exit 1; \
	fi

.PHONY: test
test:
	go test $(GO_UNIT_PKGS)

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