# syntax=docker/dockerfile:1.7

FROM golang:1.26.7-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

RUN apk upgrade --no-cache libcrypto3 libssl3 && \
    apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 app && \
    adduser -S -D -H -u 10001 -G app app && \
    mkdir -p /app/reports && \
    chown 10001:10001 /app/reports

WORKDIR /app

COPY --from=builder /out/seo-auditor ./seo-auditor

USER 10001:10001

ENTRYPOINT ["/app/seo-auditor"]
