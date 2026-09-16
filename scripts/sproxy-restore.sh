#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# sproxy-restore.sh — 从 sproxy-backup.sh 的备份 tar 恢复 storage_root（多租户布局）
#
# 校验：备份 manifest 中的版本必须与当前 CHANGELOG 首个版本一致（拒绝跨版本恢复，
# 防止旧布局数据覆盖新布局）。manifest 缺失或解析失败时拒绝恢复（fail-closed）。
#
# 用法：
#   scripts/sproxy-restore.sh --backup <tar.gz> [--target <dir>]
#
# 默认 target 为当前目录；target 不存在时自动创建。

set -euo pipefail

BACKUP_TAR=""
TARGET_DIR="."

usage() {
  sed -n '/^# 用法/,/^# 默认/p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backup)
      [[ $# -ge 2 && -n "${2:-}" ]] || { echo "--backup 需要一个 tar.gz 路径" >&2; exit 2; }
      BACKUP_TAR="$2"; shift 2 ;;
    --target)
      [[ $# -ge 2 && -n "${2:-}" ]] || { echo "--target 需要一个目录" >&2; exit 2; }
      TARGET_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1（-h 查看用法）" >&2; exit 2 ;;
  esac
done

[[ -n "$BACKUP_TAR" ]] || { echo "--backup 必填（tar.gz 路径）" >&2; exit 2; }
[[ -f "$BACKUP_TAR" ]] || { echo "备份文件不存在：$BACKUP_TAR" >&2; exit 2; }
tar -tzf "$BACKUP_TAR" >/dev/null 2>&1 || { echo "不是有效的 tar.gz：$BACKUP_TAR" >&2; exit 2; }

# 解出 manifest 校验
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
tar -xzf "$BACKUP_TAR" -C "$TMP" manifest.json 2>/dev/null || {
  echo "备份缺少 manifest.json，拒绝恢复" >&2; exit 1
}

# manifest 版本
MANIFEST_VERSION=$(sed -nE 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$TMP/manifest.json" | head -1)
[[ -n "$MANIFEST_VERSION" ]] || { echo "manifest 缺 version 字段，拒绝恢复" >&2; exit 1; }

# 当前版本（与 backup 脚本同一解析逻辑）
CURRENT_VERSION="dev"
CHANGELOG_FILE="${CHANGELOG:-CHANGELOG.md}"
if [[ -f "$CHANGELOG_FILE" ]]; then
  parsed=$(sed -nE -e 's/^## \[([0-9]+\.[0-9]+\.[0-9]+)\].*/\1/p' "$CHANGELOG_FILE" | head -1)
  [[ -n "$parsed" ]] && CURRENT_VERSION="$parsed"
fi

if [[ "$MANIFEST_VERSION" != "$CURRENT_VERSION" ]]; then
  echo "版本不匹配，拒绝恢复：备份版本 $MANIFEST_VERSION ≠ 当前版本 $CURRENT_VERSION" >&2
  exit 1
fi

mkdir -p "$TARGET_DIR"
tar -xzf "$BACKUP_TAR" -C "$TARGET_DIR" --exclude manifest.json
echo "恢复完成：$TARGET_DIR（版本 $MANIFEST_VERSION）"
