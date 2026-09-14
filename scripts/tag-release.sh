#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# tag-release.sh — 按 CHANGELOG.md 的版本列表生成根与嵌套模块 tag
#
# 为什么需要：Go 官方要求嵌套 module 的 tag 形如 <module-path>/vX.Y.Z
# （cmd/sproxy → cmd/sproxy/vX.Y.Z，见 https://go.dev/doc/modules/managing-source），
# 根 module 则只认 `vX.Y.Z`。没有嵌套 tag 时 `go get` 无法解析该 module 的版本。
#
# 用法（默认干跑、零副作用；请在仓库根目录执行）：
#   scripts/tag-release.sh                     # 干跑：打印将创建/跳过的 tag 与目标提交
#   scripts/tag-release.sh --version 0.11.0    # 只处理一个版本
#   scripts/tag-release.sh --apply             # 本地创建 annotated tag（**不推送**）
#   scripts/tag-release.sh --apply --push      # 创建并显式推送这些 tag（不可逆，需人工确认；
#                                              # --push 必须与 --apply 同时使用）
#
# 目标提交规则：优先取已存在的根 tag `vX.Y.Z` 指向的提交（保证嵌套 tag 与根 tag 同源，
# 不会因后续补充提交而漂移）；根 tag 不存在时才回落到「该版本日期当天最后一个提交」。
#
# 安全：只推送本脚本计划内的 tag（逐条显式 refspec），**绝不**使用 `git push --tags`。

set -euo pipefail

DRY_RUN=1
DO_PUSH=0
ONLY_VERSION=""

usage() {
  sed -n '/^# 用法/,/^# 安全/p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --apply)   DRY_RUN=0; shift ;;
    --push)    DO_PUSH=1; shift ;;
    --version) ONLY_VERSION="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1（-h 查看用法）" >&2; exit 2 ;;
  esac
done

if [[ $DO_PUSH -eq 1 && $DRY_RUN -eq 1 ]]; then
  echo "--push 必须与 --apply 一起使用（单用 --push 仍是干跑，把推送意图落空）" >&2
  exit 2
fi

if [[ -n "$ONLY_VERSION" && ! "$ONLY_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "--version 需形如 X.Y.Z，收到：$ONLY_VERSION" >&2
  exit 2
fi

CHANGELOG="${CHANGELOG:-CHANGELOG.md}"
[[ -f "$CHANGELOG" ]] || { echo "找不到 $CHANGELOG（请在仓库根目录执行）" >&2; exit 2; }
git rev-parse --git-dir >/dev/null 2>&1 || { echo "当前目录不是 git 仓库" >&2; exit 2; }

# 版本 -> 日期：兼容两种段标题格式：
#   历史（本仓手写）：    `## [0.11.0] - 2026-09-14`
#   release-please 产出：`## [0.12.0](https://.../compare/...) (2026-09-15)`
# 用 sed 而非 grep -oE：grep 无匹配时退出码 1 会在 set -e 下直接终止脚本，
# 使下方的“解析不到版本”报告永远不可达。
versions=$(sed -nE \
  -e 's/^## \[([0-9]+\.[0-9]+\.[0-9]+)\] - ([0-9]{4}-[0-9]{2}-[0-9]{2}).*$/\1 \2/p' \
  -e 's/^## \[([0-9]+\.[0-9]+\.[0-9]+)\]\([^)]*\) \(([0-9]{4}-[0-9]{2}-[0-9]{2})\).*$/\1 \2/p' \
  "$CHANGELOG" | sort -V)
[[ -n "$versions" ]] || {
  echo "$CHANGELOG 未解析到任何版本段（支持 '## [X.Y.Z] - YYYY-MM-DD' 与 '## [X.Y.Z](url) (YYYY-MM-DD)'）" >&2
  exit 2
}

plan_tags=()
plan_commits=()
while read -r v d; do
  [[ -z "$v" ]] && continue
  [[ -n "$ONLY_VERSION" && "$v" != "$ONLY_VERSION" ]] && continue
  if git rev-parse -q --verify "refs/tags/v$v^{commit}" >/dev/null; then
    c=$(git rev-parse "refs/tags/v$v^{commit}")
  else
    c=$(git log --until="$d 23:59:59" --format=%H -1)
  fi
  [[ -n "$c" ]] || { echo "版本 $v 找不到目标提交（日期 $d）" >&2; exit 1; }
  plan_tags+=("v$v" "cmd/sproxy/v$v" "cmd/sclient/v$v")
  plan_commits+=("$c" "$c" "$c")
done <<< "$versions"

[[ ${#plan_tags[@]} -gt 0 ]] || { echo "没有匹配的版本（--version ${ONLY_VERSION:-}）" >&2; exit 2; }

created=()
i=0
while [[ $i -lt ${#plan_tags[@]} ]]; do
  tag="${plan_tags[$i]}"; commit="${plan_commits[$i]}"; i=$((i + 1))
  if git rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null; then
    printf 'SKIP  %-26s 已存在 -> %s\n' "$tag" "$(git rev-parse --short "refs/tags/$tag^{commit}")"
  else
    printf 'TAG   %-26s -> %s\n' "$tag" "$(git rev-parse --short "$commit")"
    created+=("$tag")
  fi
done

if [[ $DRY_RUN -eq 1 ]]; then
  printf '\n[dry-run] 计划创建 %d 个 tag，未做任何改动。加 --apply 创建；再加 --push 显式推送。\n' "${#created[@]}"
  exit 0
fi

for tag in ${created[@]+"${created[@]}"}; do
  i=0
  while [[ $i -lt ${#plan_tags[@]} ]]; do
    if [[ "${plan_tags[$i]}" == "$tag" ]]; then
      commit="${plan_commits[$i]}"
      break
    fi
    i=$((i + 1))
  done
  git tag -a "$tag" "$commit" -m "Release $tag"
  printf 'CREATED %s\n' "$tag"
done

if [[ ${#created[@]} -eq 0 ]]; then
  echo "没有需要创建的 tag（全部已存在）。"
fi

if [[ $DO_PUSH -eq 1 && ${#created[@]} -gt 0 ]]; then
  printf '\n推送以下 %d 个 tag 到 origin（显式 refspec，不用 --tags）：\n' "${#created[@]}"
  printf '  %s\n' "${created[@]}"
  git push origin "${created[@]}"
fi

echo "完成。"
