# sclient 实现 fileclient.sh 全部客户端功能 Spec

## Why
当前 sclient 为空壳（仅含空 main 函数），而 fileclient.sh 提供了完整的文件上传/下载/删除/列表/配置管理功能。需要将 fileclient.sh 的功能用 Go 实现到 sclient 中，提供跨平台、无外部依赖（curl/jq）的客户端工具。

## What Changes
- 实现 sclient 命令行客户端，支持 upload/download/delete/list/config/version/help 子命令
- 使用 YAML 配置文件（~/.sclient.yaml）替代 shell 的 ~/.fileclient.conf
- 使用 Go 标准库实现 MD5 计算，无需依赖外部工具
- 使用 Go 的 net/http 发送请求，无需依赖 curl
- 使用 encoding/json 解析响应，无需依赖 jq
- 不涉及服务端（sproxy）的任何修改

## Impact
- Affected specs: add-cmd-binaries-build（sclient 从空壳变为可用程序）
- Affected code: `cmd/sclient/main.go`（重写），可能新增 `cmd/sclient/` 下的辅助文件

---

## ADDED Requirements

### Requirement: 子命令路由
系统 SHALL 支持以下子命令，未匹配时显示帮助信息：
- `upload <file1> [file2...]` — 上传一个或多个文件
- `download <filename> [output]` — 下载文件
- `delete <filename>` — 删除文件
- `list` — 列出服务器文件
- `config [show|set <key> <value>]` — 配置管理
- `version` — 显示版本信息
- `help` — 显示帮助信息

#### Scenario: 无参数时显示帮助
- **WHEN** 用户不带任何参数运行 sclient
- **THEN** 显示帮助信息并退出

#### Scenario: 未知子命令
- **WHEN** 用户输入未知子命令
- **THEN** 显示错误提示和帮助信息

### Requirement: 配置文件管理
系统 SHALL 使用 `~/.sclient.yaml` 作为配置文件，支持以下字段：
- `server_url` — 服务器地址（默认 `http://localhost:18080`）
- `upload_endpoint` — 上传端点（默认 `/upload`）
- `download_endpoint` — 下载端点（默认 `/download`）
- `delete_endpoint` — 删除端点（默认 `/delete`）
- `check_md5` — 是否启用 MD5 校验（默认 `true`）
- `timeout` — 请求超时秒数（默认 `300`）

#### Scenario: 首次运行自动创建默认配置
- **WHEN** 配置文件不存在
- **THEN** 自动创建包含默认值的配置文件

#### Scenario: config show 显示当前配置
- **WHEN** 用户执行 `sclient config show`
- **THEN** 打印所有配置项及其当前值

#### Scenario: config set 修改配置
- **WHEN** 用户执行 `sclient config set server_url http://example.com:8080`
- **THEN** 更新配置文件中的对应字段

### Requirement: 文件上传
系统 SHALL 支持单文件和批量文件上传，流程如下：
1. 计算文件 MD5
2. 发送 POST multipart/form-data 请求，携带 `X-File-MD5` 头
3. 解析 JSON 响应，显示上传结果

#### Scenario: 单文件上传成功
- **WHEN** 用户执行 `sclient upload document.pdf`
- **THEN** 计算文件 MD5，上传文件，显示成功信息

#### Scenario: 批量上传
- **WHEN** 用户执行 `sclient upload a.txt b.txt`
- **THEN** 依次上传每个文件，显示每个文件的上传结果

#### Scenario: 文件不存在
- **WHEN** 用户指定的文件路径不存在
- **THEN** 显示错误信息并跳过该文件

#### Scenario: 禁用 MD5 校验
- **WHEN** 用户使用 `--no-md5` 标志上传
- **THEN** 跳过 MD5 计算，不发送 `X-File-MD5` 头

### Requirement: 文件下载
系统 SHALL 支持文件下载，流程如下：
1. 发送 GET 请求到 `/download?filename=<name>`
2. 将响应体保存到本地文件
3. 可选：下载后计算本地文件 MD5 并与服务器响应头中的 MD5 对比

#### Scenario: 下载成功
- **WHEN** 用户执行 `sclient download report.pdf`
- **THEN** 下载文件到当前目录，显示文件大小

#### Scenario: 指定输出路径
- **WHEN** 用户执行 `sclient download report.pdf -o /tmp/out.pdf`
- **THEN** 下载文件到指定路径

#### Scenario: 下载后 MD5 校验
- **WHEN** 服务器响应包含 `X-File-MD5` 头且 `check_md5` 为 true
- **THEN** 计算下载文件 MD5 并与服务器值对比，显示校验结果

### Requirement: 文件删除
系统 SHALL 支持文件删除，流程如下：
1. 计算本地文件 MD5（如指定了本地文件路径）
2. 发送 POST 请求到 `/delete?filename=<name>`，携带 `X-File-MD5` 头
3. 解析 JSON 响应，显示删除结果

#### Scenario: 删除成功
- **WHEN** 用户执行 `sclient delete oldfile.txt`
- **THEN** 发送删除请求，显示删除结果

### Requirement: 文件列表
系统 SHALL 支持列出服务器上的文件：
1. 发送 GET 请求到 `/api/files`
2. 解析并显示文件列表

#### Scenario: 列出文件
- **WHEN** 用户执行 `sclient list`
- **THEN** 显示服务器上的文件列表

### Requirement: 全局选项
系统 SHALL 支持以下全局选项：
- `-s, --server <URL>` — 覆盖配置文件中的服务器地址
- `--no-md5` — 禁用 MD5 校验
- `-o, --output <path>` — 指定下载输出路径
- `-v, --verbose` — 详细输出模式

#### Scenario: --server 覆盖配置
- **WHEN** 用户使用 `-s http://other:8080` 选项
- **THEN** 本次命令使用指定服务器地址，不影响配置文件

### Requirement: 版本信息
系统 SHALL 在 `version` 子命令中显示版本号、构建时间、当前配置的服务器地址和 MD5 校验状态。

#### Scenario: 显示版本
- **WHEN** 用户执行 `sclient version`
- **THEN** 显示版本号和构建信息

### Requirement: 进度显示
系统 SHALL 在上传和下载过程中显示实时进度（已传输字节数/总大小）。

#### Scenario: 上传显示进度
- **WHEN** 上传大文件
- **THEN** 终端显示上传进度

#### Scenario: 下载显示进度
- **WHEN** 下载文件
- **THEN** 终端显示下载进度