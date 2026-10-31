# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY third_party/chromedp ./third_party/chromedp
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    GOMAXPROCS=2 GOMEMLIMIT=256MiB CGO_ENABLED=0 GOOS=linux \
    go build -p=1 -tags timetzdata -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache --upgrade \
        libcrypto3=3.5.9-r0 \
        libssl3=3.5.9-r0 && \
    addgroup -S -g 10001 app && \
    adduser -S -D -H -u 10001 -G app app && \
    mkdir -p /app/reports && \
    chown 10001:10001 /app/reports

WORKDIR /app

COPY --from=builder /out/seo-auditor ./seo-auditor
COPY LICENSE /usr/share/licenses/seo-auditor/LICENSE
COPY third_party/chromedp/LICENSE /usr/share/licenses/seo-auditor/chromedp-LICENSE
COPY internal/seo/fonts/LICENSE /usr/share/licenses/seo-auditor/LiberationSans-LICENSE
COPY internal/render/vendor/web-vitals.LICENSE /usr/share/licenses/seo-auditor/web-vitals-LICENSE

USER 10001:10001

ENTRYPOINT ["/app/seo-auditor"]
