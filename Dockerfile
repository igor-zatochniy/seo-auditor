# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY third_party/chromedp ./third_party/chromedp
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    GOMAXPROCS=2 GOMEMLIMIT=256MiB CGO_ENABLED=0 GOOS=linux \
    go build -p=1 -tags timetzdata -trimpath -ldflags="-s -w" -o /out/seo-auditor .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

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
