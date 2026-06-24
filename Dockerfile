FROM golang:1.26-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/ .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X main.Version=${VERSION}" -trimpath -o hubproxy .

FROM alpine

WORKDIR /app

COPY --from=builder /app/hubproxy .
COPY --from=builder /app/config.toml .

CMD ["./hubproxy"]
