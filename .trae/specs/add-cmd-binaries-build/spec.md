# 新增 cmd 命令与自动构建 Spec

## Why
为项目提供标准的可执行入口与一致的构建方式，便于本地开发与交付。

## What Changes
- 新增 cmd 目录，包含两个子命令入口：
  - cmd/sproxy：服务端可执行程序
  - cmd/sclient：客户端可执行程序
- 新增 Makefile：
  - 自动发现并编译 cmd/* 下的命令
  - 将产物输出到 build/bin/<name>[.exe]
  - 支持单命令构建与清理
- 不涉及现有包的破坏性修改

## Impact
- Affected specs: 构建与发布能力
- Affected code:
  - 顶层 Makefile（新增）
  - 新增目录：cmd/sproxy、cmd/sclient（main 包）

## ADDED Requirements
### Requirement: 提供标准命令入口与构建输出
系统应当提供两个命令入口，并支持一键构建到固定目录。

#### Scenario: make build 成功构建全部命令
- WHEN 在项目根目录运行 make build
- THEN 在 build/bin 下出现 sproxy 和 sclient（二进制）
- AND 在 Windows 下文件名带 .exe 后缀

#### Scenario: make build-sproxy 构建单一命令
- WHEN 运行 make build-sproxy
- THEN 仅生成 build/bin/sproxy[.exe]

#### Scenario: 自动发现新命令
- WHEN 在 cmd 下新增子目录 cmd/foo 且包含 main 包
- THEN make build 自动编译并生成 build/bin/foo[.exe]，无需改 Makefile

## MODIFIED Requirements
无

## REMOVED Requirements
无

