#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# sproxy-restore_test.sh — scripts/sproxy-restore.sh 的回归测试（纯 bash + 临时目录夹具）
#
# 覆盖：① 从备份 tar 完整恢复（user/meta/share 布局齐全）；② manifest 版本不匹配时
#       拒绝恢复（exit 非 0）；③ 缺 --backup 参数报错退出；④ 目标目录不存在时自动创建。
#
# 用法：bash scripts/sproxy-restore_test.sh

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
RESTORE_SCRIPT="$SCRIPT_DIR/sproxy-restore.sh"
[[ -f "$RESTORE_SCRIPT" ]] || { echo "找不到 $RESTORE_SCRIPT" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

# 先构造备份（复用 backup 脚本）
ROOT="$WORK/storage"
mkdir -p "$ROOT/tenant1/user" "$ROOT/tenant1/meta" "$ROOT/anonymous/meta/share"
echo "hello restore" > "$ROOT/tenant1/user/file.txt"
echo '{"ak":"sk"}' > "$ROOT/tenant1/meta/credentials.json"
echo '{"token":"abc"}' > "$ROOT/anonymous/meta/share/x.json"

OUT="$WORK/out"
mkdir -p "$OUT"
bash "$SCRIPT_DIR/sproxy-backup.sh" --storage-root "$ROOT" --output "$OUT" >/dev/null
TARBALL=$(ls "$OUT"/sproxy-backup-*.tar.gz | head -1)
[[ -n "$TARBALL" ]] || fail "前置备份失败：无 tar.gz"

# ① 完整恢复
TARGET="$WORK/restored"
bash "$RESTORE_SCRIPT" --backup "$TARBALL" --target "$TARGET" >/dev/null
[[ -f "$TARGET/tenant1/user/file.txt" ]] || fail "恢复后缺 user 桶文件"
[[ "$(cat "$TARGET/tenant1/user/file.txt")" == "hello restore" ]] || fail "恢复内容不一致"
[[ -f "$TARGET/tenant1/meta/credentials.json" ]] || fail "恢复后缺凭据文件"
[[ -f "$TARGET/anonymous/meta/share/x.json" ]] || fail "恢复后缺分享文件"
[[ -d "$TARGET/tenant1" ]] || fail "恢复后缺租户目录"

# ② 版本不匹配拒绝恢复：篡改 manifest 版本
tar -xzf "$TARBALL" -C "$WORK" manifest.json
sed -i 's/"version": "[^"]*"/"version": "999.0.0"/' "$WORK/manifest.json"
BAD_TAR="$WORK/bad.tar.gz"
tar czf "$BAD_TAR" -C "$WORK" manifest.json -C "$ROOT" .
set +e
bash "$RESTORE_SCRIPT" --backup "$BAD_TAR" --target "$WORK/bad-target" >/dev/null 2>&1
rc=$?
set -e
[[ $rc -ne 0 ]] || fail "版本不匹配应拒绝恢复"

# ③ 缺 --backup 报错
set +e
bash "$RESTORE_SCRIPT" --target "$WORK/x" >/dev/null 2>&1
rc=$?
set -e
[[ $rc -ne 0 ]] || fail "缺 --backup 应报错退出"

# ④ 目标目录不存在自动创建
TARGET2="$WORK/new-target/sub"
bash "$RESTORE_SCRIPT" --backup "$TARBALL" --target "$TARGET2" >/dev/null
[[ -f "$TARGET2/tenant1/user/file.txt" ]] || fail "目标目录自动创建后未恢复文件"

echo "PASS: sproxy-restore.sh 回归测试通过（4 条断言）"
