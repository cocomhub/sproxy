#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# tag-release_test.sh — scripts/tag-release.sh 的回归测试（纯 bash + 临时 git 夹具）
#
# 覆盖：① 计划同时覆盖根与两个嵌套 module；② 干跑不产生任何 tag；
#       ③ 已存在的根 tag 变为 SKIP，且嵌套 tag 与根 tag 同源（不被后续提交带偏）；
#       ④ --apply 只创建 tag、不推送。
#
# 用法：make test-tag-release（或直接 bash scripts/tag-release_test.sh）

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TAG_SCRIPT="$SCRIPT_DIR/tag-release.sh"
[[ -f "$TAG_SCRIPT" ]] || { echo "找不到 $TAG_SCRIPT" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cd "$WORK"
git init -q .
git config user.email "test@example.com"
git config user.name "test"
git config commit.gpgsign false
git config tag.gpgsign false
git symbolic-ref HEAD refs/heads/master

commit_at() { # <iso-date> <msg>
  GIT_AUTHOR_DATE="$1" GIT_COMMITTER_DATE="$1" git commit -q --allow-empty -m "$2"
}

commit_at "2026-01-01T10:00:00" "feat: a"
commit_at "2026-02-01T10:00:00" "feat: b"
commit_at "2026-03-01T10:00:00" "fix: c"

cat > CHANGELOG.md <<'MD'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-02-01

## [0.1.0] - 2026-01-01
MD

fail() { echo "FAIL: $1" >&2; exit 1; }

# ① + ② 干跑：根 + 两个嵌套 module 都必须出现在计划里，且不创建任何 tag
out=$("$TAG_SCRIPT" --dry-run)
echo "--- dry-run #1 ---"
echo "$out"
for t in v0.1.0 v0.2.0 cmd/sproxy/v0.1.0 cmd/sproxy/v0.2.0 cmd/sclient/v0.1.0 cmd/sclient/v0.2.0; do
  grep -Fq "TAG   $t" <<<"$out" || fail "计划缺少 $t"
done
[[ -z "$(git tag)" ]] || fail "dry-run 不应创建任何 tag"

# ③ 根 tag 已存在 ⇒ SKIP；嵌套 tag 与根 tag 指向同一提交
c1=$(git log --until="2026-01-01 23:59:59" --format=%H -1)
git tag -a v0.1.0 "$c1" -m "Release v0.1.0"
out=$("$TAG_SCRIPT" --dry-run)
echo "--- dry-run #2 ---"
echo "$out"
grep -Fq "SKIP  v0.1.0" <<<"$out" || fail "已存在的根 tag 应 SKIP"
proxy_line=$(grep -F "cmd/sproxy/v0.1.0" <<<"$out" | head -1)
[[ "$proxy_line" == *"$(git rev-parse --short "$c1")"* ]] \
  || fail "嵌套 tag 应与根 tag 同源（期望 $c1）"

# ④ --apply 只创建、不推送（夹具无远端；若误推送会因无 remote 失败）
"$TAG_SCRIPT" --apply --version 0.1.0 >/dev/null
git rev-parse -q --verify "refs/tags/cmd/sproxy/v0.1.0" >/dev/null \
  || fail "--apply 未创建 cmd/sproxy/v0.1.0"
git rev-parse -q --verify "refs/tags/cmd/sclient/v0.1.0" >/dev/null \
  || fail "--apply 未创建 cmd/sclient/v0.1.0"
[[ "$(git rev-parse "refs/tags/cmd/sproxy/v0.1.0^{commit}")" == "$c1" ]] \
  || fail "cmd/sproxy/v0.1.0 未指向根 tag 的提交"
[[ -z "$(git tag -l 'v0.2.0')" ]] || fail "--version 0.1.0 不应创建 v0.2.0"

echo "PASS: tag-release.sh 回归测试通过（6 条断言）"
