#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# sproxy-backup_test.sh — scripts/sproxy-backup.sh 的回归测试（纯 bash + 临时目录夹具）
#
# 覆盖：① 备份产出 tar.gz 且内含 manifest.json；② manifest 含版本号与时间戳；
#       ③ tar 内包含 user 桶文件与 meta 桶凭据/分享文件；④ 缺 storage-root 参数报错退出；
#       ⑤ --include-audit 可选参数不破坏备份。
#
# 用法：bash scripts/sproxy-backup_test.sh

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
BACKUP_SCRIPT="$SCRIPT_DIR/sproxy-backup.sh"
[[ -f "$BACKUP_SCRIPT" ]] || { echo "找不到 $BACKUP_SCRIPT" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# 构造多租户 storage_root 夹具
ROOT="$WORK/storage"
mkdir -p "$ROOT/tenant1/user" "$ROOT/tenant1/meta" "$ROOT/anonymous/meta/share"
echo "hello backup" > "$ROOT/tenant1/user/file.txt"
echo '{"ak":"sk"}' > "$ROOT/tenant1/meta/credentials.json"
echo '{"token":"abc"}' > "$ROOT/anonymous/meta/share/x.json"

OUT="$WORK/out"
mkdir -p "$OUT"

fail() { echo "FAIL: $1" >&2; exit 1; }

# ① 基本备份：产出 tar.gz
"$BACKUP_SCRIPT" --storage-root "$ROOT" --output "$OUT" >/dev/null
TARBALL=$(ls "$OUT"/sproxy-backup-*.tar.gz 2>/dev/null | head -1)
[[ -n "$TARBALL" ]] || fail "未产出 tar.gz"

# ② manifest.json 内嵌在 tar 内，含版本号与 storage_root
tar -tzf "$TARBALL" | grep -q "manifest.json" || fail "tar 内无 manifest.json"
tar -xzf "$TARBALL" -C "$WORK" manifest.json
grep -q '"version"' "$WORK/manifest.json" || fail "manifest 缺 version 字段"
grep -q '"created_at"' "$WORK/manifest.json" || fail "manifest 缺 created_at 字段"

# ③ tar 内含 user 桶文件与 meta 桶凭据/分享文件
tar -tzf "$TARBALL" | grep -q "tenant1/user/file.txt" || fail "tar 内缺 user 桶文件"
tar -tzf "$TARBALL" | grep -q "tenant1/meta/credentials.json" || fail "tar 内缺凭据文件"
tar -tzf "$TARBALL" | grep -q "anonymous/meta/share/x.json" || fail "tar 内缺分享文件"

# ④ 缺 storage-root 参数 → exit 非 0
set +e
"$BACKUP_SCRIPT" --output "$OUT" >/dev/null 2>&1
rc=$?
set -e
[[ $rc -ne 0 ]] || fail "缺 storage-root 应报错退出"

# ⑤ --include-audit 不破坏备份
"$BACKUP_SCRIPT" --storage-root "$ROOT" --output "$OUT" --include-audit >/dev/null
TARBALL2=$(ls -t "$OUT"/sproxy-backup-*.tar.gz | head -1)
tar -tzf "$TARBALL2" | grep -q "manifest.json" || fail "--include-audit 后缺 manifest"

echo "PASS: sproxy-backup.sh 回归测试通过（5 条断言）"
