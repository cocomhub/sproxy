#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# regression-audit —— 「功能不遗失」回归审计（硬规则 1.17 的机械化）。
#
# 判据是**被删掉的东西**：收敛 / 改公共默认值 / 替换实现之后，最容易丢的就是
# ① 导出符号（能力入口消失）、② 能力相关字段与语义档位（超时、连接池、重定向…）。
# 本脚本用 `git show <commit> -U0` 把这两类删除项抽出来，并要求逐条回答
# 「它是能力遗失，还是测试重命名/薄封装/等价替换」——不能靠记性。
#
# 用法：
#   scripts/regression-audit.sh                 # 审计 origin/master 最近 12 个提交
#   scripts/regression-audit.sh <sha> [<sha>…]  # 审计指定提交（squash commit 或范围）
#   scripts/regression-audit.sh --current       # 只审计**当前工作区**（未提交改动）
#
# 退出码：0 = 完成扫描（人工据此逐条判定）；2 = 参数/环境错误。
#
# 依赖：git、grep（无需网络）。

set -uo pipefail

cd "$(git rev-parse --show-toplevel)" 2>/dev/null || { echo "不在 git 仓库内" >&2; exit 2; }

# 被删除的**导出符号**（能力入口）。
export_re='^-[^-].*(func|type|const|var)[[:space:]]+[A-Z][A-Za-z0-9_]*'
# 被删除的**能力相关字段/档位**（消失的语义）。
capab_re='(Transport|Timeout|Jar|CheckRedirect|Retry|Concurrency|MaxIdleConns|MaxConns|Isolated|BufferSize|ChunkSize|MaxVersions|Retention|Attempts|Backoff)'

scan_diff() { # $1 = 标题, $2 = 产生 diff 的命令
	local title="$1"; shift
	echo "########## $title"
	local diff
	diff="$("$@" 2>/dev/null)" || { echo "  (无法取得 diff)"; return; }
	if [ -z "$diff" ]; then echo "  (无改动)"; return; fi

	echo "  --- 被删除的导出符号（能力入口？）"
	echo "$diff" | grep -E "$export_re" | head -15 | sed 's/^/  /' || echo "    (无)"
	echo "  --- 被删除的能力相关字段/档位（语义变化？）"
	echo "$diff" | grep -E '^-[^-]' | grep -E "$capab_re" | head -15 | sed 's/^/  /' || echo "    (无)"
}

if [ "${1:-}" = "--current" ]; then
	scan_diff "当前工作区（未提交改动）" git diff -U0 HEAD
else
	shas="${*:-}"
	if [ -z "$shas" ]; then
		shas="$(git log --format=%H -12 origin/master 2>/dev/null || true)"
		[ -n "$shas" ] || { echo "取不到 origin/master 提交列表（先 git fetch）" >&2; exit 2; }
		echo "（未指定提交 ⇒ 审计 origin/master 最近 12 个提交）"
	fi
	for s in $shas; do
		subject="$(git log -1 --format='%s' "$s" 2>/dev/null || echo "$s")"
		scan_diff "$(echo "$s" | cut -c1-10) $subject" git show "$s" -U0 --format=
	done
fi

echo
echo "########## 当前实测（收敛后的客观状态）"
echo "  - 裸默认池构造点（应为 0；R57 门禁另守）："
grep -rn 'httpc.NewHTTPClient(httpc.Options{})\|httpc.New(httpc.Options{})' --include=*.go pkg cmd 2>/dev/null \
	| grep -v _test | sed 's/^/    /' || echo "    (0 ✓)"
echo "  - CloseIdleConnections 调用点（必须自有池；R58 守）："
grep -rn 'CloseIdleConnections()' --include=*.go pkg cmd 2>/dev/null | grep -v _test \
	| grep -vE 'pkg/httpc/|pkg/netutil/' | sed 's/^/    /' || echo "    (无)"
echo
echo "⇒ 对上面每条删除项，逐条回答：「能力遗失」还是「测试重命名/薄封装/等价替换」；"
echo "   有行级依据才可判定，禁止「应该没事」。"
