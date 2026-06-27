FROM node:22-alpine AS web-builder
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN go mod tidy
COPY --from=web-builder /internal/api/webdist ./internal/api/webdist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o homeforge ./cmd/homeforge

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/homeforge /usr/local/bin/homeforge
RUN mkdir -p /data /etc/homeforge
VOLUME ["/data", "/etc/homeforge"]
EXPOSE 8123 1883
ENTRYPOINT ["homeforge", "/etc/homeforge/config.yaml"]
