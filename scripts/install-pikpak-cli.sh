#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================
# PikPak 官方 CLI 安装脚本（跨 darwin/linux/windows 通用）
#
# 用法:
#   bash scripts/install-pikpak-cli.sh [install_dir]
#   INSTALL_DIR=~/bin bash scripts/install-pikpak-cli.sh
#
# 行为:
#   1. 从官方发布清单拉取当前版本
#   2. 按平台/架构下载对应二进制（windows 自动 .exe）
#   3. 安装到 install_dir（默认 ~/.local/bin）
#   4. 打印安装路径
#
# 之后登录：pikpak auth login（浏览器授权码）
# ============================================================
set -euo pipefail

CFG_URL="https://config.mypikpak.com/config/v1/command_line?client=global"
INSTALL_DIR="${INSTALL_DIR:-${1:-$HOME/.local/bin}}"

# --- 平台/架构检测 → 资产名 ---
OS="$(uname -s 2>/dev/null || echo unknown)"
ARCH="$(uname -m 2>/dev/null || echo unknown)"
case "$OS" in
  Darwin) os_key="darwin" ;;
  Linux)  os_key="linux" ;;
  MINGW*|MSYS*|CYGWIN*) os_key="windows" ;;
  *) echo "error: unsupported OS $OS" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) arch_key="amd64" ;;
  arm64|aarch64) arch_key="arm64" ;;
  *) echo "error: unsupported arch $ARCH" >&2; exit 1 ;;
esac

asset="pikpak_${os_key}_${arch_key}"
[ "$os_key" = "windows" ] && asset="${asset}.exe"

# --- 拉发布清单拿资产 URL ---
echo "info: fetching release config..."
cfg="$(curl -fsSL --max-time 30 "$CFG_URL" 2>/dev/null || { echo "error: cannot fetch $CFG_URL" >&2; exit 1; })"
# 用 grep 提取该资产的 URL（无 jq 依赖）
asset_url="$(printf '%s' "$cfg" | tr '{},' '\n\n\n' | grep -B1 "$asset" | grep -oE 'https://[^"]+' | head -1 || true)"
if [ -z "$asset_url" ]; then
  echo "error: asset $asset not found in release config" >&2
  exit 1
fi

# --- 下载安装 ---
mkdir -p "$INSTALL_DIR"
dest="$INSTALL_DIR/$asset"
if [ -f "$dest" ] && [ -s "$dest" ]; then
  echo "info: already installed at $dest"
else
  echo "info: downloading $asset_url -> $dest"
  curl -fsSL --max-time 300 -o "$dest" "$asset_url"
  [ "$os_key" != "windows" ] && chmod +x "$dest"
fi

echo "success: PikPak CLI installed at $dest"
echo "next: run '${dest##*/} auth login' and approve in browser"

# 可选：软链到 PATH
if [ -d "$HOME/bin" ] && [ "${HOME}/bin" != "$INSTALL_DIR" ]; then
  ln -sf "$dest" "$HOME/bin/${asset}"
fi
