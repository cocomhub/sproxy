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
# 二进制归 root 所有（0755 默认）：应用用户 sproxy 只需执行，不应可改写自身二进制
# （防应用被入侵后篡改映像内可执行文件；运行时写入只在 /app/storage VOLUME）。
COPY ${TARGETPLATFORM}/sproxy /usr/local/bin/sproxy
COPY ${TARGETPLATFORM}/sclient /usr/local/bin/sclient
USER sproxy

EXPOSE 18083

ENV SPROXY_ADDR=:18083
ENV SPROXY_STORAGE_ROOT=/app/storage

VOLUME ["/app/storage"]

ENTRYPOINT ["/usr/local/bin/sproxy"]
