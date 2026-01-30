# Build
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN go build -o /app/server main.go

# Runtime
FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates curl

# Install mc client (download at build time)
RUN curl -fsSL -o /usr/local/bin/mc https://dl.min.io/client/mc/release/linux-amd64/mc \
  && chmod +x /usr/local/bin/mc

COPY --from=builder /app/server /app/server
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/entrypoint.sh"]
