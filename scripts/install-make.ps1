<#
 Copyright 2026 The Cocomhub Authors. All rights reserved.
 SPDX-License-Identifier: Apache-2.0
#>

# scripts/install-make.ps1 — 在 Windows 上安装 GNU Make
# 首次构建前或在 CI Windows runner 上执行

$makeCheck = Get-Command make -ErrorAction SilentlyContinue
if ($makeCheck) {
    Write-Host "GNU Make already installed."
    exit 0
}

if (-not (Get-Command choco -ErrorAction SilentlyContinue)) {
    Write-Host "Chocolatey not found. Installing Chocolatey..."
    Set-ExecutionPolicy Bypass -Scope Process -Force
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
    Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
}

choco install make -y --no-progress
