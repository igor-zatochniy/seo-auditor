# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.26.7-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -tags timetzdata -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

RUN apk add --no-cache --upgrade \
        libcrypto3=3.5.8-r0 \
        libssl3=3.5.8-r0 && \
    addgroup -S -g 10001 app && \
    adduser -S -D -H -u 10001 -G app app && \
    mkdir -p /app/reports && \
    chown 10001:10001 /app/reports

WORKDIR /app

COPY --from=builder /out/seo-auditor ./seo-auditor

USER 10001:10001

ENTRYPOINT ["/app/seo-auditor"]
