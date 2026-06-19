# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
WORKDIR /build

# 缓存依赖层
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 多架构静态编译
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" CGO_ENABLED=0 \
    go build -ldflags="-w -s" -o /build/sproxy ./cmd/sproxy/ && \
    go build -ldflags="-w -s" -o /build/sclient ./cmd/sclient/

# ─── runtime ───────────────────────────────────────────
FROM alpine:3.21

# 非 root 用户
RUN apk add --no-cache ca-certificates tzdata && adduser -D -h /app sproxy

WORKDIR /app
USER sproxy

COPY --from=builder --chown=sproxy:sproxy /build/sproxy .
COPY --from=builder --chown=sproxy:sproxy /build/sclient .

EXPOSE 18083

ENV SPROXY_ADDR=:18083
ENV SPROXY_UPLOADS_DIR=/app/uploads

VOLUME ["/app/uploads"]

ENTRYPOINT ["/app/sproxy"]
