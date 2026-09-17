# BaiduPCS V3：volume 通用化（多用户 × 每用户多盘） 设计文档

> **状态：** 已确认（2026-09-17 20:30 用户定案）
> **前置：** P4（T1–T7）已实现 baidupcs 基座（sync.FS 适配 + VolumeBackend + syncmgr 集成 + 系统盘多盘），本文档在其上做 **volume 统一管理**的通用化设计。
> **路径：** P4 不合并，V3 在 P4 分支续做（吸收 T4 装配 + T7 Disks）；**volume backend 可插拔（plugin）**；用户卷 API 独立 PR。

## 目标（用户需求 2026-09-17）

1. **百度云用户可多个**，每个下面可挂**不同的根目录作为盘**（同一百度账号不同盘根 = 不同卷）
2. 盘用**唯一标识 id** 区分
3. **用户自有盘**放每个用户的 meta 下维护（`<tenant>/<user>/meta/...`，仿 SproxySig 凭据 store）
4. **现有实现作为服务系统盘**（扩展：config 静态配置）
5. **也要支持配多个百度云用户的多个根目录**（config 静态）
6. **后续扩展 volume 类型也是类似实现**——扩展性和可维护性优先
7. 寻址：`remote.volume` 带 owner
8. **volume 才是逻辑卷，用卷来管理**（不新建 disk 概念）

## 核心设计

### 1. volume.Volume 扩展（纯域模型，向后兼容）

```go
// pkg/volume/volume.go
type Volume struct {
    Name     string            // 卷名（唯一 id）
    Type     string            // 卷类型：local(缺省/空) | baidupcs | s3 | webdav ...
    RootDir  string            // 本地根（本地卷 = 存储根；外部卷 = 本地中间态 staging 基目录）
    Capacity int64             // 0 = 不限制
    ACL      ACL               // 卷 ACL（现有）
    Extra    map[string]any    // 类型特有配置（baidupcs: BDUSS/root/binary_path/local_root...）
}
```

- `Type == ""` 视为 `local`（缺省兼容，零迁移）
- `Extra` 为 `map[string]any`（JSON 友好，meta store 持久化用）

### 2. registry.Set 支持外部卷

```go
// pkg/volume/registry/set.go
type ExternalBackend interface {
    FS() sync.FS        // 外部卷的同步视图（StorageFS）
    Close() error
}

type Set struct {
    volumes  []volume.Volume          // 卷元数据（含 Type/Extra，现有）
    roots    map[string]*storage.Root // 本地卷句柄（现有）
    external map[string]ExternalBackend // 外部卷句柄（新增，按卷名）
    ...
}

func NewSet(volumes []volume.Volume, roots map[string]*storage.Root, external map[string]ExternalBackend, defaultName string) *Set
```

- 卷路由（Authorize/AllowedVolumes/选卷）不变（只消费 Volume 元数据，Type 无关）
- 新增 `External(name string) ExternalBackend` 查询（工厂按 remote.volume 查 registry）

### 3. 卷来源（两层）

```
系统卷（config volumes[]）     配置静态，多百度云用户 × 多根目录
   volumes:
     - name: "sys-baidu-1"      # 唯一 id
       type: "baidupcs"         # 新字段
       root: "/data/baidupcs-1" # 本地中间态基目录
       extra: {bduss: "...", baidu_root: "/disk1"}  # baidupcs 特有
     - name: "sys-baidu-2"
       type: "baidupcs"
       ...

用户卷（每用户 meta store）     动态，用户自有盘（仅 owner 可用）
   <tenant>/<user>/meta/volume/<name>.json
   { "name": "my-disk-1", "type": "baidupcs", "extra": {...}, ... }
```

- 系统卷：所有用户可用（ACL 控制，现有机制）
- 用户卷：仅 owner（任务 Owner 校验匹配；跨用户视为不存在）

### 4. volume backend plugin 注册表（可插拔）

```go
// pkg/volume/registry：backend 插件注册表（type → 构造器，可插拔扩展）
type BackendFactory func(ctx context.Context, v volume.Volume) (ExternalBackend, error)

var backendFactories = map[string]BackendFactory{}

// RegisterBackend 注册后端类型构造器（包 init 或装配层显式注册；未来 s3/webdav 只加注册）
func RegisterBackend(typ string, f BackendFactory)

// 工厂按 remote.volume → registry 查卷 → 按 v.Type 查注册表 → 构造器
```

- **baidupcs backend 是插件**：`pkg/baidupcs` 或装配层 `RegisterBackend("baidupcs", newBaidupcsBackend)`
- baidupcs backend：从 `v.Extra` 取 BDUSS/baidu_root/binary_path → `NewStorage` → `NewVolumeBackend` → `StorageFS`（WithQuota 注入）
- 新 volume 类型（s3/webdav...）= 新 plugin 注册，**不改装配/工厂核心**（扩展性/可维护性）

### 5. 寻址与权限

- `remote.volume` = 卷名（唯一 id）
- 用户卷权限：任务创建时校验 `task.Owner == 卷.Owner`（用户卷）或卷 ACL 允许（系统卷）
- `volume.Volume` 加 `Owner string`（空 = 系统卷/共享卷）？——或在 registry 外部层维护 owner→卷映射

## 实施拆分

**V3 核心（当前 P4 分支续做，一个 PR）：**
| 子任务 | 内容 | 影响 |
|---|---|---|
| V3a | volume.Volume + Type/Extra + config VolumeConfig 支持 type（吸收 T7 Disks） | pkg/volume + pkg/server |
| V3b | registry.Set 支持 external backends + backend plugin 注册表 | pkg/volume/registry |
| V3c | baidupcs backend 插件 + 工厂按 Type 查 registry（吸收 T4 装配） | cmd/sproxy + pkg/baidupcs |
| V3d | 装配改造（系统卷统一）+ e2e + 文档 | cmd/sproxy + 测试 |

**独立 PR（V3 后）：** 用户卷 meta store + 管理 API（POST /api/volume CRUD，仅 owner）

## 影响面（大改预警）

- `pkg/volume`（域模型 + Type/Extra + registry external）
- `pkg/server`（config 解析 + 装配 + 用户卷 meta store + API）
- `pkg/syncexec`（工厂按 Type 分派查 registry）
- `pkg/baidupcs`（backend 构造器适配）
- 现有本地卷零迁移（Type 缺省 local）

## 待用户确认

1. 用户卷是否也需要 API（CRUD 经 `POST /api/volume` 等）还是仅配置/文件直管？
2. 系统卷 + 用户卷命名空间冲突？（同名卷：用户卷优先？还是系统卷 ACL 控制？）
3. T8 是否作为独立 PR（P4 T1–T7 先合并基座）？
