# Volume 框架 V3：通用卷模型 + 可插拔后端

> **状态：** 已实现（2026-09-17，feat/volume-framework PR）
> **背景：** sproxy 原卷模型仅支持本地文件系统卷（`volume.Volume{Name/RootDir/Capacity/ACL}` + `storage.OpenRoot`）。百度网盘后端（baidupcs）需要「像管理本地卷一样管理网盘卷、多用户每用户多盘」，且后续要扩展更多 volume 类型（S3/WebDAV 等）——本设计把卷模型升级为**通用卷模型 + 可插拔后端**。

## 目标

1. `volume.Volume` 携带**类型**（Type）与**类型特有配置**（Extra）——卷是逻辑卷，多盘/多后端统一用卷管理。
2. 后端**可插拔**（plugin）：按 Type 注册构造器，新后端只加注册，不改装配/路由核心。
3. **零迁移**：现有本地卷配置不变（Type 缺省 local）。

## 设计

### 1. volume.Volume 扩展（纯域模型）

```go
// pkg/volume/volume.go
const TypeLocal = "local" // 本地卷类型字面量；空串与 TypeLocal 等义（零迁移）

type Volume struct {
    Name     string            // 卷名（唯一 id）
    Type     string            // 卷后端类型：空/"local" = 本地卷；外部 = 如 "baidupcs"
    RootDir  string            // 本地根（本地卷 = 存储根；外部卷 = ""（无本地根））
    Capacity int64             // 0 = 不限制
    ACL      ACL               // 卷 ACL（现有，与 Type 无关）
    Extra    map[string]any    // 类型特有配置（JSON 友好；本地卷恒 nil）
}
```

- `Type` 不参与 `Authorize`/选卷（纯元数据，决定「怎么装配」而非「谁能用」）。
- `Extra` 由后端构造器读取（如 baidupcs 的 bduss/baidu_root/binary_path）。

### 2. registry 外部卷 + plugin 注册表

```go
// pkg/volume/registry/backend.go
type ExternalBackend interface {
    FS() syncpkg.FS   // 外部卷的同步视图（sync.FS），供 syncexec 工厂消费
    Close() error     // 释放后端资源（幂等）
}

type BackendFactory func(ctx context.Context, v volume.Volume) (ExternalBackend, error)

func RegisterBackend(typ string, f BackendFactory) // 可插拔注册（重复/空 type → panic）
func NewBackend(ctx context.Context, v volume.Volume) (ExternalBackend, error) // 按 Type 分派
```

```go
// pkg/volume/registry/set.go
type Set struct {
    volumes  []volume.Volume
    roots    map[string]*storage.Root        // 本地卷句柄（仅 Type local）
    external map[string]ExternalBackend      // 外部卷句柄（按卷名）
    pools    map[string]*quota.Pool          // 容量池（外部卷同样建池，入账统一）
    ...
}

func NewSet(volumes, roots, external, pools, defaultName) *Set
func (vs *Set) External(name string) ExternalBackend
```

### 3. 装配按 Type 分派（pkg/server/volumes.go assembleVolumes）

```
for each vc in cfg.Volumes:
    if vc.Type == "" || vc.Type == "local":   → 本地卷：MkdirAll + storage.OpenRoot + roots 持有
    else:                                     → registry.NewBackend(ctx, v) → external 持有
    两者都建容量 Pool + ACL 解析
```

- **默认卷（volumes[0]）必须是本地卷**（需要 storage.Root；外部首卷 → 装配错误 fail-closed）。
- **未知 Type（未注册后端）→ 装配失败 fail-closed**（不回落 local——回落会静默把外部卷当本地目录打开）。
- `config_validate`：外部卷跳过 Root 非空检查（无本地根）、跳过重复 root 检查。

### 4. 配置（config.example.yaml）

```yaml
volumes:
  - name: "main"              # 默认卷（本地）
    type: "local"             # 缺省即 local（零迁移）
    root: "./storage"
  - name: "baidudisk"         # 外部卷（需后端已注册）
    type: "baidupcs"          # 如 baidupcs 后端
    extra: { bduss: "...", baidu_root: "/disk1" }
```

## 未来后端接入步骤（扩展性）

1. 实现 `ExternalBackend`（FS() 返回该后端的 sync.FS 视图 + Close()）。
2. `registry.RegisterBackend("mytype", factory)`（在包 init 或装配层注册）。
3. 配置 `volumes[]` 加 `type: "mytype"` + `extra: {...}`。
4. 装配自动按 Type 分派——**不改 assembleVolumes/路由/ACL/容量逻辑**。

## 测试

- `pkg/volume/volume_test.go`：Type 缺省 local / Extra JSON 往返 / Authorize 不受 Type 影响。
- `pkg/volume/registry/backend_test.go`：RegisterBackend（重复/空 panic）+ NewBackend 分派（未知 type fail-closed）。
- `pkg/volume/registry/set_test.go`：external map 持有/查询。
- `pkg/server/volumes_test.go`：装配分派（本地零迁移 / 外部构造 / 未知 type fail-closed / 外部默认卷 fail-closed）。
- `pkg/server/volumes_e2e_test.go`：端到端（注册 → 装配 → Set.External → FS() 往返数据 + Close 幂等）。
- 变异验证：Set.External 去掉 → e2e 红；后端分派去掉 → 外部卷装配红。
