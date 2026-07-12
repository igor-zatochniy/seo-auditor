# syntax=docker/dockerfile:1.7

FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 app && \
    adduser -S -D -H -u 10001 -G app app

WORKDIR /app

COPY --from=builder /out/seo-auditor ./seo-auditor

USER 10001:10001

ENTRYPOINT ["/app/seo-auditor"]
