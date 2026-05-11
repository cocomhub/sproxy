# 检查 downserver 功能覆盖情况 Spec

## Why
需要确认 `cmd/downserver`（main.go + fileclient.sh）的功能是否已经通过 `sproxy`（服务端）和 `sclient`（客户端）完整实现，以决定是否可以废弃旧的 downserver 代码。

## What Changes
- 对比分析 downserver 服务端功能与 sproxy 的覆盖情况
- 对比分析 fileclient.sh 客户端功能与 sclient 的覆盖情况
- 输出覆盖结论和差异清单

## Impact
- Affected specs: 无（纯分析任务）
- Affected code: `cmd/downserver/`, `cmd/sproxy/`, `cmd/sclient/`, `internal/handlers/`

---

## 分析结论

### 一、服务端功能对比：downserver/main.go vs sproxy

| downserver 功能 | sproxy 覆盖 | 说明 |
|---|---|---|
| `/upload` 文件上传 + MD5 校验 | ✅ 已覆盖 | 逻辑一致，增强为结构化日志（slog） |
| `/download` 文件下载 | ✅ 已覆盖 | 逻辑一致 |
| `/delete` 文件删除 + MD5 校验 | ✅ 已覆盖 | 逻辑一致 |
| `/{host}/{filepath...}` 代理转发 | ✅ 已覆盖 | 增强：新增 host 白名单校验、hop-by-hop header 剥离 |
| `/bandwidth` 带宽查询 | ✅ 已覆盖 | 逻辑一致 |
| FileMD5/MD5 工具函数 | ✅ 已覆盖 | 实现一致 |
| 带宽统计 goroutine | ✅ 已覆盖 | 逻辑一致 |
| 优雅关闭（signal handling） | ✅ 已覆盖 | 逻辑一致 |
| `-uploads-dir` 命令行参数 | ✅ 已覆盖 | 同参数名 |
| 端口 :18080 | ✅ 已覆盖 | 通过 `-addr` 或 YAML 配置，更灵活 |

**sproxy 额外增强功能（downserver 不具备）：**
- `/healthz` 健康检查端点
- `/version` 版本信息端点
- YAML 配置文件支持（日志级别、超时、host 白名单等）
- 结构化日志（slog）
- Host 白名单访问控制
- Hop-by-hop header 剥离（符合 HTTP 代理规范）
- 可配置的 Server Timeouts（Read/Write/Idle/ReadHeader）
- 可配置的 MaxHeaderBytes

**结论：服务端功能 100% 覆盖，sproxy 是 downserver 的超集。**

---

### 二、客户端功能对比：fileclient.sh vs sclient

| fileclient.sh 功能 | sclient 覆盖 | 说明 |
|---|---|---|
| `upload` 单文件上传 + MD5 | ❌ 未实现 | sclient 的 main.go 为空 |
| `upload` 批量上传 | ❌ 未实现 | sclient 的 main.go 为空 |
| `download` 文件下载 + MD5 校验 | ❌ 未实现 | sclient 的 main.go 为空 |
| `delete` 文件删除 + MD5 校验 | ❌ 未实现 | sclient 的 main.go 为空 |
| `list` 列出服务器文件 | ❌ 未实现 | sclient 的 main.go 为空 |
| `config` 配置管理（show/set） | ❌ 未实现 | sclient 的 main.go 为空 |
| `version` 版本信息 | ❌ 未实现 | sclient 的 main.go 为空 |
| `help` 帮助信息 | ❌ 未实现 | sclient 的 main.go 为空 |
| MD5 计算（跨平台） | ❌ 未实现 | sclient 的 main.go 为空 |
| 进度条显示 | ❌ 未实现 | sclient 的 main.go 为空 |
| 配置文件持久化（~/.fileclient.conf） | ❌ 未实现 | sclient 的 main.go 为空 |

**结论：客户端功能 0% 覆盖，sclient 目前为空壳。**

---

### 三、总体结论

| 组件 | 覆盖状态 |
|---|---|
| downserver/main.go → sproxy | ✅ **完全覆盖**（sproxy 是超集） |
| fileclient.sh → sclient | ❌ **未覆盖**（sclient 为空） |

**建议：**
1. downserver/main.go 可以安全废弃，sproxy 已完全替代并增强
2. sclient 需要实现 fileclient.sh 的全部客户端功能（upload/download/delete/list/config/version/help/MD5/进度条）