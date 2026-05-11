# sproxy 核心重构与优化 Spec

## Why
当前 main.go 体量较大、职责混杂；日志在 log 与 slog 间混用；-config 指定的配置未被实际加载；传输带宽统计存在重复计数风险；HTTP 客户端与服务端缺少超时与安全边界控制。需要通过结构化重构与基础设施完善，提升可维护性、稳定性与可观测性。

## What Changes
- 项目结构调整：拆分 config、server、handlers、version 等模块，主函数仅负责装配与启动
- 统一日志：使用 slog，支持级别与输出格式配置；贯穿 request-id
- 配置管理：从 -config 加载 YAML，合并命令行 flag，提供默认值与校验
- HTTP 服务端：显式 mux；设置 ReadHeader/Read/Write/Idle 超时与最大头大小；优雅退出
- 代理转发：仅允许 allow-list 主机；为下游请求设置超时；剔除 hop-by-hop 头
- 带宽统计：修复重复累计问题，确保统计准确且为非负；保留现有 /bandwidth 接口行为
- API 一致性：/upload、/download、/delete 使用统一 JSON 响应辅助函数；保留现有语义与状态码
- 运维接口：新增 /healthz 与 /version
- 文档与示例：提供 config.yaml 示例与使用说明
- 无状态兼容：保持现有路由与标志不变，配置为空时回退到旧行为（无 **BREAKING**）

## Impact
- 影响能力：启动流程、日志、配置、代理安全、带宽统计、可观测性
- 影响代码：重构 main.go；新增/调整 config、server、handlers、utils；新增文档示例

## ADDED Requirements
### Requirement: 配置加载与合并
系统 SHALL 从 `-config` 指定路径加载 YAML 配置，并与命令行 flag 合并，字段至少包含：
- `addr`（监听地址，默认 `:18080`）
- `uploads_dir`（上传目录，默认 `./uploads`）
- `allowed_hosts`（允许转发的主机列表，留空表示不限制）
- `http_timeouts`（服务端与代理客户端的超时设置）
- `log_level`、`log_format`（text/json）

#### Scenario: 成功加载配置
- WHEN 用户提供合法的 YAML 配置或不提供配置
- THEN 程序以配置与默认值合并后的结果运行

### Requirement: 统一日志
系统 SHALL 统一使用 slog 输出；支持在配置中设置日志级别与格式；在处理请求时输出 request-id。

#### Scenario: 日志生效
- WHEN 发起任何 API 请求
- THEN 相关日志以 slog 输出，包含 request-id 与关键字段

### Requirement: 转发安全控制
系统 SHALL 在 `transfer` 路由仅转发至 `allowed_hosts` 列表中的主机；列表为空时允许任意主机（保持兼容）。系统 SHALL 剔除 hop-by-hop 头。

#### Scenario: 非允许主机
- WHEN 请求 host 不在 allow-list
- THEN 返回 403 并记录安全日志

### Requirement: 连接超时与稳健性
系统 SHALL 为服务端设置合理超时（如 ReadHeader/Read/Write/Idle）；为代理 http.Client 设置请求超时与 Transport 连接限制。

#### Scenario: 下游超时
- WHEN 下游在超时阈值内未响应
- THEN 立即中止并返回 504/502，附错误信息

### Requirement: 带宽统计修正
系统 SHALL 修复带宽统计重复计数，确保统计严格按写入客户端的字节数累计一次；`/bandwidth` 继续返回当前 KB/s 数值文本。

#### Scenario: 统计准确
- WHEN 有连续的下载流量
- THEN `/bandwidth` 随时间变化且非负，无明显翻倍错误

### Requirement: 健康与版本接口
系统 SHALL 提供 `/healthz`（200 OK）与 `/version`（输出 Version/BuildAt）。

#### Scenario: 健康检查
- WHEN 访问 `/healthz`
- THEN 返回 200 与简单文本

## MODIFIED Requirements
### Requirement: 上传/下载/删除接口的响应与日志
保持现有 API 路径与语义；统一使用 JSON 辅助函数输出成功与错误响应；使用 slog 记录关键字段与错误栈，不更改返回状态码与字段含义。

## REMOVED Requirements
无

