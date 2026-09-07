#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# 起 hashicorp/vault dev 容器 → 跑 L2/L3 Vault 集成测试 → 清理容器。
# 无 docker 时提示（vault_integration 用例将 t.Skip，不失败）。
#
# 用法：make test-vault  （或直接 bash scripts/test-vault.sh）
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v docker >/dev/null 2>&1; then
  echo "docker 不可用，跳过 Vault 集成测试（vault_integration 用例将 t.Skip）" >&2
  exit 0
fi

name="sproxy-vault-test"
# 清理可能的残留容器。
docker rm -f "$name" >/dev/null 2>&1 || true

echo "启动 Vault dev 容器（hashicorp/vault:latest, root token=root, :8200）..."
docker run -d --rm --name "$name" \
  -p 8200:8200 \
  -e VAULT_DEV_ROOT_TOKEN_ID=root \
  hashicorp/vault:latest >/dev/null

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# 等 Vault 就绪（健康检查 200；30s 超时后仍不可达 → 明确失败）。
ready=0
for _i in $(seq 1 30); do
  if curl -sf http://127.0.0.1:8200/v1/sys/health >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "错误：Vault 容器 30s 内未就绪（health 非 200）" >&2
  exit 1
fi

echo "Vault 就绪，运行 L2/L3 集成测试..."
VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root \
  go test -race -count=1 -timeout=120s ./pkg/accesskey/ -run "Vault|L2|L3" -v
