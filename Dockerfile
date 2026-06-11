# syntax=docker/dockerfile:1.7

FROM golang:1.26.4-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 app && \
    adduser -S -D -H -u 10001 -G app app

WORKDIR /app

COPY --from=builder /out/seo-auditor ./seo-auditor

USER 10001:10001

ENTRYPOINT ["/app/seo-auditor"]
