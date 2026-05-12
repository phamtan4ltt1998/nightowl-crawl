## Stage 1: build
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /fetcher ./cmd/server

## Stage 2: minimal runtime
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /fetcher /fetcher
COPY sources.yaml ./
RUN chown -R app:app /app
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -sf http://localhost:8080/health || exit 1
ENTRYPOINT ["/fetcher"]
