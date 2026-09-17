# 用户卷（User Volumes）：per-owner volume meta store + 管理 API 设计文档

> **状态：** 已确认（2026-09-17 21:45 用户定案）
> **定案：** ① 用户卷仅外部类型（baidupcs 等 backend 插件）；② 运行中引用删除 409；③ 独立卷容量（vol_capacity，不计 owner 配额）。
> **前置：** P4 + V3 已合并（master `862572df`）：volume 通用模型（Type/Extra）+ 可插拔 backend（RegisterBackend）+ baidupcs 系统盘（volumes[] type=baidupcs）。

## 目标（用户需求 2026-09-17）

1. **百度云用户可多个**，每个下面可挂**不同的根目录作为盘**（同一百度账号不同盘根 = 不同卷）
2. 盘用**唯一标识 id** 区分
3. **用户自有盘**放每个用户的 meta 下维护（`<tenant>/<user>/meta/...`，仿 SproxySig 凭据 store）
4. 现有实现作为**服务系统盘**（config volumes[]，已合并）
5. 后续扩展 volume 类型也是类似实现（plugin 可插拔，已合并）
6. 寻址：`remote.volume` 带 owner（任务 Owner 校验匹配）
7. **用户卷需要管理 API**（用户确认：POST /api/volume CRUD，仅 owner 可用）

## 核心设计

### 1. 用户卷存储（meta store）

```
<tenant>/<user>/meta/volume/<name>.json
{
  "name": "my-disk-1",          // 卷名（唯一，owner 内）
  "type": "baidupcs",           // 卷类型（复用 backend 插件）
  "owner": "alice",             // 归属用户
  "extra": {                    // 类型特有配置（baidupcs: bduss/baidu_root/binary_path/local_root）
    "bduss": "...",
    "baidu_root": "/disk1",
    "local_root": "/data/baidupcs/alice-disk1"
  }
}
```

- 位置：`<storage_root>/<owner>/meta/volume/`（仿凭据 store 布局）
- 原子写（tmp + rename），重启后扫描恢复
- 仅 owner 可用（任务 Owner 校验匹配；跨用户 404 防枚举）

### 2. registry.Set 动态扩展（运行时注册用户卷）

```go
// pkg/volume/registry/set.go
// Set 加并发安全的外部卷动态注册：
func (vs *Set) AddExternalVolume(v volume.Volume, be ExternalBackend) error  // 加锁；重名拒绝
func (vs *Set) RemoveExternalVolume(name string) error                        // 加锁；删除 + Close
func (vs *Set) External(name string) ExternalBackend                         // 现有查询（改加锁读）
```

- 用户卷 API 创建 → store 落盘 → `Set.AddExternalVolume`（同步任务工厂立即可查）
- 删除 → `Set.RemoveExternalVolume`（Close 后端）→ store 删文件
- 并发安全：Set 目前无 mutex（装配后只读）——Add/Remove/External 需加 RWMutex

### 3. 用户卷管理 API

```
POST   /api/volumes/user          创建用户卷（JSON: {name, type, extra}）
GET    /api/volumes/user          列出我的用户卷（owner 过滤）
DELETE /api/volumes/user?name=<n> 删除用户卷（owner 校验）
```

- 认证：SproxySig / api_keys（owner 从请求派生，仿现有 handler）
- 创建校验：type 已注册 backend；extra 符合类型要求（baidupcs 需 bduss/binary_path）
- 删除：同步任务正在引用该卷 → **409 拒绝**（明确提示，任务结束后才可删）
- 容量：**独立卷容量**（vol_capacity 存卷描述，不计 owner 配额——用户确认）
- ACL：用户卷默认私有（owner 专属），无 ACL 配置（系统卷才有）

### 4. 同步任务寻址（remote.volume 带 owner）

- `sync_remotes[].kind=baidupcs + volume=<用户卷名>`——任务 Owner 字段匹配卷.Owner
- 工厂查 Set.External（系统卷 + 用户卷统一）：`Set.External(name)` 命中用户卷 → FS 可用
- 跨用户访问：任务 Owner ≠ 卷.Owner → 404（防枚举）

### 5. 扩展性（plugin 复用）

- 用户卷 type=baidupcs → 复用现有 RegisterBackend("baidupcs") 构造器
- 未来 type=s3/webdav → 用户卷同样支持（无需改用户卷框架）

## 实施拆分

| 子任务 | 内容 |
|---|---|
| U1 | registry.Set 动态 Add/Remove ExternalVolume（RWMutex 并发安全） |
| U2 | 用户卷 meta store（<tenant>/<user>/meta/volume/ CRUD + 原子写 + 扫描恢复） |
| U3 | 管理 API（POST/GET/DELETE /api/volumes/user，owner 校验 + type backend 校验） |
| U4 | 同步任务寻址（工厂查 Set.External 已统一；任务 Owner 匹配卷.Owner 校验） |
| U5 | e2e + 文档 |

## 影响面

- `pkg/volume/registry`（Set 动态方法 + RWMutex）
- `pkg/server`（用户卷 store + 管理 API + 装配扫描恢复）
- `pkg/syncmgr`（任务创建校验 owner↔卷归属）
- baidupcs 无需改（backend 插件复用）

## 待确认

1. 用户卷是否也支持本地（type=local）？还是仅外部类型（baidupcs）？
2. 删除语义：运行中引用的卷（同步任务在跑）——409 拒绝 vs 标记删除延迟？
3. 容量限制：用户卷是否也受 owner 配额（owner_quotas）约束？
