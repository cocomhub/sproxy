#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# check-test-files.sh — 验证所有 Go 包都有测试文件
# 使用方法： scripts/check-test-files.sh <packages...>
# .notestignore 中列出的包免检（支持 `*` glob）
#
# 历史缺陷（2026-09-14 修复，两处都让这道门禁**永远绿**）：
#   1. Makefile 的 notest 目标不带参数调用本脚本 ⇒ for 循环零次迭代 ⇒ 直接打印 OK；
#      现改为调用方传包列表，且本脚本对空参数**fail-closed**（宁可报错也不要假绿）。
#   2. 忽略清单用 `find -not -path` 实现：被忽略时输出为空，反而被判为"未忽略"，
#      于是 .notestignore 从未生效；现改为对包路径做 glob 正向匹配。

set -euo pipefail

IGNORE_FILE=".notestignore"

# 门禁必须 fail-closed：调用方漏传包列表时要显式失败，否则就是"永远绿"的假门禁。
if [[ $# -eq 0 ]]; then
  echo "FAIL: 未收到任何包路径（门禁会空转）——用法: scripts/check-test-files.sh <packages...>" >&2
  exit 1
fi

IGNORE_PATTERNS=()
if [[ -f "$IGNORE_FILE" ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="$(echo "$line" | xargs)"
    [[ -z "$line" ]] && continue
    IGNORE_PATTERNS+=("$line")
  done < "$IGNORE_FILE"
fi

# is_ignored <pkg>：包路径（去掉 ./ 前缀后）命中 .notestignore 任一 glob 模式即视为免检。
is_ignored() {
  local pkg="${1#./}"
  local pat
  if [[ ${#IGNORE_PATTERNS[@]} -eq 0 ]]; then
    return 1
  fi
  for pat in "${IGNORE_PATTERNS[@]}"; do
    pat="${pat#./}"
    # shellcheck disable=SC2053  # RHS 故意不加引号，以便按 glob 匹配
    if [[ "$pkg" == $pat ]]; then
      return 0
    fi
  done
  return 1
}

exit_code=0
missing_count=0
missing_list=""

for pkg in "$@"; do
  pkg="${pkg%/}"
  [[ -z "$pkg" ]] && continue
  [[ "$pkg" == "." ]] && continue
  [[ -d "$pkg" ]] || continue

  if is_ignored "$pkg"; then
    continue
  fi

  test_files="$(find "$pkg" -maxdepth 1 -name '*_test.go' -print -quit 2>/dev/null || true)"
  if [[ -n "$test_files" ]]; then
    continue
  fi

  echo "FAIL: $pkg has no test files" >&2
  exit_code=1
  missing_count=$((missing_count + 1))
  missing_list="$missing_list $pkg"
done

if [[ $exit_code -eq 0 ]]; then
  echo "OK: all packages have test files"
else
  echo "FAIL: $missing_count package(s) missing test files:$missing_list" >&2
fi

exit $exit_code
