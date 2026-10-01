<#
 Copyright 2026 The Cocomhub Authors. All rights reserved.
 SPDX-License-Identifier: Apache-2.0
#>

# ============================================================
# PikPak 官方 CLI 安装脚本（Windows PowerShell 版）
# 用法:  powershell -ExecutionPolicy Bypass -File scripts/install-pikpak-cli.ps1 [install_dir]
# ============================================================
param(
  [string]$InstallDir = "$env:LOCALAPPDATA\pikpak-cli"
)

$ErrorActionPreference = "Stop"
$cfgUrl = "https://config.mypikpak.com/config/v1/command_line?client=global"

# 平台/架构
$osKey = "windows"
switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
  "X64"   { $archKey = "amd64" }
  "Arm64" { $archKey = "arm64" }
  default { Write-Error "unsupported arch" }
}
$asset = "pikpak_windows_${archKey}.exe"

Write-Host "info: fetching release config..."
$cfg = Invoke-RestMethod -Uri $cfgUrl -TimeoutSec 30
$assetUrl = $null
foreach ($a in $cfg.values.command_line.assets) {
  if ($a.name -eq $asset) { $assetUrl = $a.browser_download_url; break }
}
if (-not $assetUrl) { Write-Error "asset $asset not found" }

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$dest = Join-Path $InstallDir $asset
if ((Test-Path $dest) -and ((Get-Item $dest).Length -gt 0)) {
  Write-Host "info: already installed at $dest"
} else {
  Write-Host "info: downloading $assetUrl -> $dest"
  Invoke-WebRequest -Uri $assetUrl -OutFile $dest -TimeoutSec 300
}

Write-Host "success: PikPak CLI installed at $dest"
Write-Host "next: run '$dest auth login' and approve in browser"
