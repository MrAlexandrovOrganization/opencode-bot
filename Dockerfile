FROM golang:1.26-alpine AS builder

RUN apk add --no-cache protobuf-dev && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY proto/ /proto/
COPY . .

RUN mkdir -p gen/whisper && \
    protoc -I /proto \
        --go_out=gen/whisper --go_opt=paths=source_relative \
        --go-grpc_out=gen/whisper --go-grpc_opt=paths=source_relative \
        /proto/whisper.proto

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bot ./cmd/bot

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bot /bot
CMD ["/bot"]
