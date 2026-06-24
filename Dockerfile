FROM golang:1.26-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X main.Version=${VERSION}" -trimpath -o hubproxy .

FROM alpine

WORKDIR /app

COPY --from=builder /app/hubproxy .
COPY config.example.toml ./config.toml

CMD ["./hubproxy"]
