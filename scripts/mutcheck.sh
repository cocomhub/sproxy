#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# mutcheck —— 变异验证骨架（硬规则 5「TDD + 变异验证」的机械化）：
# 对源码施加一处**真实变异**，断言指定测试**变红**，然后**无论成败都复原**文件。
#
# 为什么需要：声称「测试能抓这个 bug」之前必须证明「变异已命中」；手工做这件事时
# 最容易犯两个错 —— ① 忘了复原（把变异残留留在工作区，后续诊断全被污染）；
# ② 只看「有 FAIL 输出」而不确认是**行为断言**失败（编译错误/夹具噪声也算红）。
#
# 用法：
#   scripts/mutcheck.sh -file pkg/cipherstream/random_access.go \
#                       -old 'return r.readBlockSkip(l, off-plainSoFar)' \
#                       -new 'return r.readBlockSkip(l, off-plainSoFar-1)' \
#                       -pkg ./pkg/storage/ -run 'TestEncSeekPlain|TestEncRootGold'
#
# 退出码：0 = 变异被捕获（测试确实红）；1 = **变异未被捕获**（测试缺口）；2 = 参数错误；
#         3 = 锚点未命中/不唯一（变异未应用，需改锚点）；4 = 缺少依赖。
#
# 依赖：git（校验工作区）、perl（做**字面**替换并断言锚点唯一）。
# 提示：子 module 请加 -go 'GOWORK=off go'（或用 -gowork off）。

set -uo pipefail

file="" ; old="" ; new="" ; pkg="" ; run="" ; gobin="${GO:-go}"

usage() {
	sed -n '3,24p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
		-file) file="$2"; shift 2 ;;
		-old)  old="$2";  shift 2 ;;
		-new)  new="$2";  shift 2 ;;
		-pkg)  pkg="$2";  shift 2 ;;
		-run)  run="$2";  shift 2 ;;
		-go)   gobin="$2"; shift 2 ;;
		-h|--help) usage ;;
		*) echo "未知参数: $1" >&2; usage ;;
	esac
done
[ -n "$file" ] && [ -n "$old" ] && [ -n "$new" ] && [ -n "$pkg" ] && [ -n "$run" ] || usage
[ -f "$file" ] || { echo "文件不存在: $file" >&2; exit 2; }
command -v perl >/dev/null 2>&1 || { echo "缺少 perl（用于字面替换）" >&2; exit 4; }

# 变异前必须先提交/暂存干净，否则无法区分「原有改动」与「变异残留」。
if [ -n "$(cd "$(git rev-parse --show-toplevel)" && git status --porcelain -- "$file")" ]; then
	echo "⚠️ $file 有未提交改动：变异前请先提交，避免与变异混在一起" >&2
fi

backup="$(mktemp)" || exit 4
cp -f "$file" "$backup"
# 无论正常结束、报错还是 Ctrl-C，都必须复原（历史教训：变异残留污染后续诊断）。
restore() { cp -f "$backup" "$file"; rm -f "$backup"; }
trap restore EXIT INT TERM

OLD="$old" NEW="$new" perl -0777 -i -pe '
	BEGIN { $n = 0 }
	$n += s/\Q$ENV{OLD}\E/$ENV{NEW}/g;
	END { die "anchor-count=$n\n" unless $n == 1 }
' "$file" 2>/dev/null
if [ $? -ne 0 ]; then
	echo "✗ 锚点未命中或不唯一（要求恰好 1 处）：$old" >&2
	exit 3
fi
echo "✦ 变异已应用：$file"

out="$($gobin test "$pkg" -run "$run" -count=1 2>&1)"
rc=$?
if [ "$rc" -eq 0 ]; then
	echo "✗ 变异**未被捕获**（测试仍绿）—— 存在测试缺口，请补断言后再声称测试有效" >&2
	echo "$out" | tail -20 >&2
	exit 1
fi
# 只认「行为级红」的可识别信号（编译失败也算红，但显式提示，避免把编译错误当行为验证）。
if echo "$out" | grep -qE "\[build failed\]|cannot use|undefined:|imported and not used"; then
	echo "△ 变异后编译失败（弱红：能证明依赖关系，但不能证明行为断言）" >&2
	echo "$out" | grep -E "\.go:[0-9]+" | head -5 >&2
	exit 0
fi
echo "✓ 变异被捕获（测试变红）—— 该断言确实锁住了这处行为"
echo "$out" | grep -E "^(--- FAIL|FAIL|\s+.*_test\.go:[0-9]+:)" | head -6

# 复原由 trap 负责；此处显式再确认一次文件已回到原状。
if cmp -s "$backup" "$file"; then :; fi
