# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# 供 GoReleaser dockers_v2 使用：构建上下文由 GoReleaser 准备，其中已包含
# <os>/<arch>/sproxy 与 <os>/<arch>/sclient 预编译二进制。**不要在 Dockerfile 内重新编译**
# （会重复 GoReleaser 已完成的工作，并显著拖慢镜像构建）。
#
# 本地单独 `docker build .` 不可用（上下文缺少二进制）；如需验证请用
# `goreleaser release --snapshot --clean` 或 CI 的 Release workflow。
FROM alpine:3.21

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata && adduser -D -h /app sproxy

WORKDIR /app
COPY --chown=sproxy:sproxy ${TARGETPLATFORM}/sproxy /usr/local/bin/sproxy
COPY --chown=sproxy:sproxy ${TARGETPLATFORM}/sclient /usr/local/bin/sclient
USER sproxy

EXPOSE 18083

ENV SPROXY_ADDR=:18083
ENV SPROXY_STORAGE_ROOT=/app/storage

VOLUME ["/app/storage"]

ENTRYPOINT ["/usr/local/bin/sproxy"]
