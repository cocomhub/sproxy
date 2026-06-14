# Errors

Command failures and integration errors.

---

## [ERR-20260614-001] viper.New missing ReadInConfig

**Logged**: 2026-06-14T09:00:00Z
**Priority**: high
**Status**: resolved
**Area**: config

### Summary
`cmd/sclient/tunnel.go` 使用 `viper.New()` 创建独立实例后未调用 `ReadInConfig()`，导致配置文件中的 `tunnel_key` 永远无法被读取。

### Error
tunnel 命令创建了 `viper.New()` 实例，配置了文件路径、类型、前缀和环境变量自动绑定，但遗漏了 `ReadInConfig()`。`LoadFromViper(v)` 返回的 `cfg.TunnelKey` 永远为空，用户始终收到"请先配置 tunnel_key"错误。

### Context
- RunE 中迁移 `viper.GetViper()` → `viper.New()` 时遗漏
- 代码评审发现
- 已修复：添加 `ReadInConfig()` + `ConfigFileNotFoundError` 处理
- Related Files: `cmd/sclient/tunnel.go`

### Metadata
- Reproducible: yes
- Related Files: cmd/sclient/tunnel.go
- See Also: LRN-20260614-BP1

---
**清理备注**: 此 ERR 内容已完全覆盖于 LRN-20260614-BP1，后者已 `promoted` 到 project memory。若无新的 viper.ReadInConfig 复现，下次 review 可归档此条目。
