#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# sproxy-backup.sh — 备份 sproxy storage_root（多租户布局）
#
# 布局：<tenant>/{user,cloud,archive,chunk,version,meta}/ 桶（pkg/storage/tenant.go
# featureBuckets 白名单）。本脚本全量打包 storage_root 下所有内容（含 meta 桶的
# 凭据 store 与分享持久化文件），并内嵌 manifest.json 记录版本与时间戳。
#
# 用法：
#   scripts/sproxy-backup.sh --storage-root <path> [--output <dir>] [--include-audit]
#
# 产出：<output>/sproxy-backup-<YYYYmmdd-HHMMSS>.tar.gz（含 manifest.json）。
# 恢复用 scripts/sproxy-restore.sh。

set -euo pipefail

STORAGE_ROOT=""
OUTPUT_DIR="backups"
INCLUDE_AUDIT=0

usage() {
  sed -n '/^# 用法/,/^# 产出/p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --storage-root)
      [[ $# -ge 2 && -n "${2:-}" ]] || { echo "--storage-root 需要一个路径参数" >&2; exit 2; }
      STORAGE_ROOT="$2"; shift 2 ;;
    --output)
      [[ $# -ge 2 && -n "${2:-}" ]] || { echo "--output 需要一个目录参数" >&2; exit 2; }
      OUTPUT_DIR="$2"; shift 2 ;;
    --include-audit)
      INCLUDE_AUDIT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1（-h 查看用法）" >&2; exit 2 ;;
  esac
done

[[ -n "$STORAGE_ROOT" ]] || { echo "--storage-root 必填" >&2; exit 2; }
[[ -d "$STORAGE_ROOT" ]] || { echo "storage_root 不存在：$STORAGE_ROOT" >&2; exit 2; }

mkdir -p "$OUTPUT_DIR"

TIMESTAMP=$(date +%Y%m%d-%H%M%S)
TARBALL="$OUTPUT_DIR/sproxy-backup-$TIMESTAMP.tar.gz"

# 版本号：优先取 buildmeta 内嵌值，回落到 CHANGELOG 首个版本段（脚本独立运行场景）。
VERSION="dev"
if [[ -f internal/buildmeta/build/dirty_info.txt ]]; then
  VERSION="dev"
fi
# 从 CHANGELOG.md 解析首个版本段（release-please 或手写格式），防版本漂移。
CHANGELOG_FILE="${CHANGELOG:-CHANGELOG.md}"
if [[ -f "$CHANGELOG_FILE" ]]; then
  parsed=$(sed -nE \
    -e 's/^## \[([0-9]+\.[0-9]+\.[0-9]+)\].*/\1/p' \
    "$CHANGELOG_FILE" | head -1)
  [[ -n "$parsed" ]] && VERSION="$parsed"
fi

# manifest.json：内嵌进 tar（在临时目录写好再打进包根）。
MANIFEST_DIR=$(mktemp -d)
trap 'rm -rf "$MANIFEST_DIR"' EXIT
cat > "$MANIFEST_DIR/manifest.json" <<JSON
{
  "tool": "sproxy-backup",
  "version": "$VERSION",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "storage_root": "$(cd "$STORAGE_ROOT" && pwd)",
  "include_audit": $([ $INCLUDE_AUDIT -eq 1 ] && echo true || echo false)
}
JSON

# 打包：storage_root 内容 + manifest.json（manifest 在包根）。
tar czf "$TARBALL" -C "$STORAGE_ROOT" . -C "$MANIFEST_DIR" manifest.json

echo "备份完成：$TARBALL（版本 $VERSION）"
