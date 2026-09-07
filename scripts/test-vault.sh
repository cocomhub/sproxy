#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# 起 hashicorp/vault dev 容器 → 跑 L2/L3 Vault 集成测试 → 清理容器。
# 无 docker 时提示（vault_integration 用例将 t.Skip，不失败）。
#
# 行为（M3/M4/M6）：
#   - 若 127.0.0.1:8200 已有健康 Vault（宿主 curl 探测）→ 直接复用，不起容器；
#   - 否则起 docker 容器（镜像钉版本：hashicorp/vault:1.18，可用 VAULT_IMAGE 覆写）；
#   - 就绪探测优先宿主 curl，宿主无 curl 时经 docker exec 容器内 wget 探测；
#   - 只跑 L2/L3 集成用例（-run '^TestVault_L[23]_'），不跑 L1 mock。
#
# 用法：make test-vault  （或直接 bash scripts/test-vault.sh）
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v docker >/dev/null 2>&1; then
  echo "docker 不可用，跳过 Vault 集成测试（vault_integration 用例将 t.Skip）" >&2
  exit 0
fi

name="sproxy-vault-test"
container_started=0
VAULT_IMAGE="${VAULT_IMAGE:-hashicorp/vault:1.18}"

# host_vault_healthy：宿主 curl 探测 127.0.0.1:8200 是否已有健康 Vault（复用）。
host_vault_healthy() {
  command -v curl >/dev/null 2>&1 || return 1
  curl -sf http://127.0.0.1:8200/v1/sys/health >/dev/null 2>&1
}

# container_vault_ready：容器起好后探测就绪（宿主 curl 优先；无 curl → docker exec wget，
# vault 镜像自带 busybox wget）。
container_vault_ready() {
  if command -v curl >/dev/null 2>&1; then
    curl -sf http://127.0.0.1:8200/v1/sys/health >/dev/null 2>&1
  else
    docker exec "$name" wget -qO- http://127.0.0.1:8200/v1/sys/health >/dev/null 2>&1
  fi
}

if host_vault_healthy; then
  echo "检测到 127.0.0.1:8200 已有健康 Vault——直接复用（不起容器）"
else
  echo "启动 Vault dev 容器（$VAULT_IMAGE, root token=root, :8200）..."
  # 清理可能的残留容器。
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --rm --name "$name" \
    -p 8200:8200 \
    -e VAULT_DEV_ROOT_TOKEN_ID=root \
    "$VAULT_IMAGE" >/dev/null
  container_started=1
  cleanup() {
    if [ "$container_started" -eq 1 ]; then
      docker rm -f "$name" >/dev/null 2>&1 || true
    fi
  }
  trap cleanup EXIT

  # 等 Vault 就绪（健康检查 200；30s 超时后仍不可达 → 明确失败）。
  ready=0
  for _i in $(seq 1 30); do
    if container_vault_ready; then
      ready=1
      break
    fi
    sleep 1
  done
  if [ "$ready" -ne 1 ]; then
    echo "错误：Vault 容器 30s 内未就绪（health 非 200）" >&2
    exit 1
  fi
fi

echo "Vault 就绪，运行 L2/L3 集成测试..."
VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root \
  go test -race -count=1 -timeout=120s ./pkg/accesskey/ -run '^TestVault_L[23]_' -v
