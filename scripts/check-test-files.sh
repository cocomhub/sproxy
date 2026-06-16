#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# check-test-files.sh — 验证所有 Go 包都有测试文件
# 使用方法： scripts/check-test-files.sh <packages...>
# .notestignore 中列出的包免检

set -euo pipefail

IGNORE_FILE=".notestignore"

EXCLUDE_ARGS=()
if [ -f "$IGNORE_FILE" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"
    line="$(echo "$line" | xargs)"
    [ -z "$line" ] && continue
    EXCLUDE_ARGS+=(-not -path "./$line")
  done < "$IGNORE_FILE"
fi

exit_code=0
missing_count=0
missing_list=""

for pkg in "$@"; do
  pkg="${pkg%/}"
  [ -z "$pkg" ] || [ "$pkg" = "." ] || [ ! -d "$pkg" ] && continue

  test_files=$(find "$pkg" -maxdepth 1 -name '*_test.go' -print -quit 2>/dev/null)
  if [ -n "$test_files" ]; then
    continue
  fi

  if [ ${#EXCLUDE_ARGS[@]} -gt 0 ]; then
    excluded_file=$(find "$pkg" -maxdepth 0 "${EXCLUDE_ARGS[@]}" -print -quit 2>/dev/null)
    [ -z "$excluded_file" ] || continue
  fi

  echo "FAIL: $pkg has no test files" >&2
  exit_code=1
  missing_count=$((missing_count + 1))
  missing_list="$missing_list $pkg"
done

if [ $exit_code -eq 0 ]; then
  echo "OK: all packages have test files"
else
  echo "FAIL: $missing_count package(s) missing test files:$missing_list" >&2
fi

exit $exit_code
