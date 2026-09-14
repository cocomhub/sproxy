#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# tag-release_test.sh — scripts/tag-release.sh 的回归测试（纯 bash + 临时 git 夹具）
#
# 覆盖：① 计划同时覆盖根与两个嵌套 module；② 干跑不产生任何 tag；
#       ③ release-please 段标题格式 `## [X.Y.Z](url) (date)` 也能解析；
#       ④ 已存在的**根** tag 变为 SKIP，嵌套 tag 与根 tag 同源（不被后续提交带偏）；
#       ⑤ 已存在的**嵌套** tag 同样 SKIP；
#       ⑥ --apply 只创建 tag、不推送；--version 只处理指定版本；
#       ⑦ 解析不到版本段时显式报错退出（exit 2）；
#       ⑧ --push 单独使用（未加 --apply）显式报错退出（exit 2）。
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

# 0.3.0 用 release-please 的段标题格式（带 compare 链接、日期在括号里）。
cat > CHANGELOG.md <<'MD'
# Changelog

## [Unreleased]

## [0.3.0](https://example.com/compare/v0.2.0...v0.3.0) (2026-03-01)

## [0.2.0] - 2026-02-01

## [0.1.0] - 2026-01-01
MD

fail() { echo "FAIL: $1" >&2; exit 1; }
run_fail() { # <期望退出码> <描述> <命令...>
  local want="$1" desc="$2"; shift 2
  set +e
  out=$("$@" 2>&1); rc=$?
  set -e
  [[ "$rc" -eq "$want" ]] || fail "$desc（期望 exit $want，实际 $rc）"
}

# ① + ② + ③ 干跑：三个版本 × 三种 tag 都在计划里（含 release-please 格式的 0.3.0），
# 且不创建任何 tag
out=$("$TAG_SCRIPT" --dry-run)
echo "--- dry-run #1 ---"
echo "$out"
for t in v0.1.0 v0.2.0 v0.3.0 \
         cmd/sproxy/v0.1.0 cmd/sproxy/v0.2.0 cmd/sproxy/v0.3.0 \
         cmd/sclient/v0.1.0 cmd/sclient/v0.2.0 cmd/sclient/v0.3.0; do
  grep -Fq "TAG   $t" <<<"$out" || fail "计划缺少 $t"
done
[[ -z "$(git tag)" ]] || fail "dry-run 不应创建任何 tag"

# ④ 根 tag 已存在 ⇒ SKIP；嵌套 tag 与根 tag 指向同一提交
c1=$(git log --until="2026-01-01 23:59:59" --format=%H -1)
git tag -a v0.1.0 "$c1" -m "Release v0.1.0"
out=$("$TAG_SCRIPT" --dry-run)
echo "--- dry-run #2 ---"
echo "$out"
grep -Fq "SKIP  v0.1.0" <<<"$out" || fail "已存在的根 tag 应 SKIP"
proxy_line=$(grep -F "cmd/sproxy/v0.1.0" <<<"$out" | head -1)
[[ "$proxy_line" == *"$(git rev-parse --short "$c1")"* ]] \
  || fail "嵌套 tag 应与根 tag 同源（期望 $c1）"

# ⑥ --apply 只创建、不推送（夹具无远端；若误推送会因无 remote 失败）；
#    --version 只处理指定版本
"$TAG_SCRIPT" --apply --version 0.1.0 >/dev/null
git rev-parse -q --verify "refs/tags/cmd/sproxy/v0.1.0" >/dev/null \
  || fail "--apply 未创建 cmd/sproxy/v0.1.0"
git rev-parse -q --verify "refs/tags/cmd/sclient/v0.1.0" >/dev/null \
  || fail "--apply 未创建 cmd/sclient/v0.1.0"
[[ "$(git rev-parse "refs/tags/cmd/sproxy/v0.1.0^{commit}")" == "$c1" ]] \
  || fail "cmd/sproxy/v0.1.0 未指向根 tag 的提交"
[[ -z "$(git tag -l 'v0.2.0')" ]] || fail "--version 0.1.0 不应创建 v0.2.0"

# ⑤ 已存在的**嵌套** tag 也应 SKIP
out=$("$TAG_SCRIPT" --dry-run)
grep -Fq "SKIP  cmd/sproxy/v0.1.0" <<<"$out" || fail "已存在的嵌套 tag cmd/sproxy/v0.1.0 应 SKIP"

# ⑦ 解析不到版本段 ⇒ 显式报错（exit 2），而不是静默通过
cat > EMPTY.md <<'MD'
# Changelog

## [Unreleased]
MD
run_fail 2 "空 CHANGELOG 应报错退出" env CHANGELOG="$WORK/EMPTY.md" "$TAG_SCRIPT" --dry-run
grep -Fq "未解析到任何版本段" <<<"$out" || fail "空 CHANGELOG 缺少明确错误信息"

# ⑧ --push 必须与 --apply 同时使用
run_fail 2 "--push 单用应报错退出" "$TAG_SCRIPT" --push

# ⑨ 无参调用 = 干跑（安全默认：不得创建任何 tag）
before_tags=$(git tag -l | sort)
out=$("$TAG_SCRIPT")
echo "--- dry-run #3 (no args) ---"
echo "$out"
after_tags=$(git tag -l | sort)
[[ "$before_tags" == "$after_tags" ]] || fail "无参调用必须为干跑（不得创建/删除任何 tag）"
grep -Fq "dry-run" <<<"$out" || fail "无参调用应打印 dry-run 计划"

echo "PASS: tag-release.sh 回归测试通过（9 条断言）"
