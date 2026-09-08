<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# X：多卷存储 + 卷 ACL 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 单节点多卷存储——每卷独立根 + 六桶布局 + 卷容量上限，写入服务端自动路由（placement 默认卷优先/可配 spread），读取跨卷定位，卷 ACL 黑白名单×默认开放/拒绝，客户端默认无感、可深入（`--volume`/`volumes`/跨卷 move）。

**架构：** 新增 `pkg/volume` 纯域包（卷/ACL/选卷排序逻辑，无 quota/无 I/O），server 装配层把旧单 `globalRoot` 语义映射到「默认卷」，新增 `volumeRoots map[卷]*storage.Root` + `volumePools map[卷]*quota.Pool`（每卷容量）。owner 全局配额（`quotaScopes`/`quotaBuckets`）保持跨卷合计语义不变；写路径双 TryReserve（owner 全局 → 目标卷）。checksum/凭据/cloud/sync 等全局账本**恒默认卷**（meta 归属不变式）。旧客户端全部端点缺省 = 默认卷/auto（零回归）。

**技术栈：** Go 1.26，纯 stdlib + 仓库已有 `pkg/storage`、`pkg/quota`、`pkg/server`。TDD。多 module：根 go.mod（主 module），`cmd/sproxy`、`cmd/sclient`、`pkg/tunnel/xfer/ext/*` 为独立子 module（本计划不触子 module，但 `make test-all`/`make lint` 须全绿）。

---

## 全局约束（每个任务必须遵守，违反即审查缺陷）

1. **单卷零回归红线**：未配置 `volumes`（或只配单卷且 root==storage_root）时行为与当前完全一致。任何任务改动不得破坏现有 integration 全量测试与既有单测。
2. **meta 归属不变式**：凭据 `credentials.json`、checksum `checksums.json`、cloud/sync/hub 状态**恒在默认卷** `<默认卷根>/<owner>/meta/`，绝不随文件所在卷分散。非默认卷的 `meta/` 不写这些全局账本。
3. **路径唯一**：owner 逻辑文件树中同一相对路径只能存在于一个卷。自动路由写入天然唯一；显式指定卷写入前查重（目标卷已有同 rel → `409`；其它卷已有同 rel → `409` + 提示所在卷）。
4. **ACL fail-closed**：显式指定卷/路由选卷/定位前都校验 owner 允许；不允许 → `403`。`allow` 未命中即拒；`deny` 命中黑名单即拒；未配 ACL = `deny`+空 owners = 默认开放。
5. **配额双账本**：owner 全局池语义保留（跨卷合计）；每卷容量池新增。写路径「owner 全局 → 目标卷」两次 TryReserve，都成功才写；失败回滚已预留（owner 全局满不换卷；卷容量满换下一候选，全满 `ErrStorageFull`）。删除双 Release。
6. **lint 0**：主 go.mod + 每个子 go.mod `golangci-lint run` 0 issues；`go fmt ./...`；SPDX 头（`addlicense`）。提交前 `make check-ci` 全绿（或至少 `make lint` + `go test -count=1 ./pkg/... ./cmd/...` + 受影响包 `-race`）。
7. 测试用临时目录模拟多卷（`t.TempDir()` 建多块假盘根），**禁止** `0.0.0.0`/`localhost` 监听；`httptest` 默认 loopback 即可。

### 执行顺序与依赖

T1 → T2 → T3（装配）→ T4（写路由）→ T5（读定位）→ T6（特征桶 + 新 API/move）→ T7（客户端/WebUI）。T3 后每个后续任务都跑单卷零回归。

### 关键精确取值（跨任务一致，勿偏离）

- 卷配置模型（viper/yaml 键）：顶层 `Config` 增 `Placement string`（`yaml:"placement"`，合法值 `prefer-default`|`spread`，缺省 `prefer-default`）、`Volumes []VolumeConfig`（`yaml:"volumes"`）。
- `VolumeConfig{ Name string \`yaml:"name"\`; Root string \`yaml:"root"\`; VolCapacity int64 \`yaml:"vol_capacity"\`; ACL *VolumeACLConfig \`yaml:"acl"\` }`。
- `VolumeACLConfig{ Mode string \`yaml:"mode"\`; Owners []string \`yaml:"owners"\` }`，Mode 合法 `allow`|`deny`，nil/省略 = `deny`。
- 装配后的卷名即默认卷名 = `cfg.Volumes[0].Name`；未配 `volumes` 时构造单卷 `{Name:"default", Root:cfg.StorageRoot}`（internal 合成，不进 yaml 往返）。
- `quota.ErrStorageFull` 作为满错误；http 映射 507/503 见现有 upload 处理（跟随现有 `sendJSONResponse` 用 `UploadResponse{Success:false,Message}`）。
- 错误码：`403 volume not allowed`、`409 volume conflict: already exists in volume <name>`、`404 not found`。
- 新端点：`GET /api/volumes`、`POST /api/volumes/move?from_volume=<v>&to_volume=<v>&filename=<rel>`。list 返回项增 `volume` 字段；list/stat/download/delete/rename 支持可选 `volume` 参数（query 或表单/头，见各任务）。

### 现有代码锚点（implementer 先读再改）

- 装配：`cmd/sproxy/root.go:683`（`SetReconciler`/Scan）；`pkg/server/handlers.go:96-106`（容器）、`:218-261`（`tenantFor`）、`:265`（`tenantOf`）、`:273`（`listTenantIDs`）、`:307`（`checksumStoreFor`）、`:380-421`（`ensureTenantQuotaLocked`）、`:471`（`quotaScopeFor`）、`:560-701`（RegisterRoutes 装配）、`:704-709`（localMux 注册）。
- 写路径 reserve 模式：读 `pkg/server/upload_handler.go` 主体 + `pkg/quota/quota_writer.go` + `pkg/server/quota_write_path_test.go`（理解现有 TryReserve→写→Commit 与时序）。
- reconcile：`pkg/server/quota_reconcile.go` 全文（`:35` 起）。
- 分块上传：`pkg/server/chunked_upload.go`、`chunked_download.go`（`/upload/init` 定卷改动点）。
- cloud：`pkg/server/cloud_download.go`、`cloud_download_handler.go`（cloud 桶默认卷绑定）；archive：`pkg/server/archive.go`。
- 客户端：`pkg/client/client.go`（`FileClient`）、`chunked.go`、`stream.go`、`dirs.go`；CLI：`cmd/sclient/*.go`（upload.go/download.go/list.go/mv.go + root.go 子命令注册）。
- WebUI：`web/static/index.html`、`app.js`、`app-render.js`、`style.css`。

---

### 任务 1：config 卷模型 + Validate + ACL 解析

**文件：**
- 修改：`pkg/server/config.go`（Config struct :346、`Default()` :446、`SetDefaults`/normalize 区 :528 附近、`Validate()` :647、`LoadFromViper` 无改动必要——mapstructure 自动收新键）
- 测试：`pkg/server/config_volumes_test.go`（新建）

**职责：** 定义卷配置类型与校验；缺省合成单卷；placement/ACL 精确值校验。不接装配。

- [ ] **步骤 1：写失败测试** `pkg/server/config_volumes_test.go`

```go
func TestVolumesConfig_DefaultsAndParse(t *testing.T) {
	// 未配 volumes → 单卷合成（name=default, root=StorageRoot）
	c := Default()
	if got := len(c.Volumes); got != 1 {
		t.Fatalf("未配 volumes 应有合成单卷, got %d", got)
	}
	if c.Volumes[0].Name != "default" || c.Volumes[0].Root != c.StorageRoot {
		t.Fatalf("合成默认卷不符: %+v", c.Volumes[0])
	}
	if c.Placement != "prefer-default" {
		t.Fatalf("缺省 placement 应为 prefer-default, got %q", c.Placement)
	}
}

func TestVolumesConfig_Validate(t *testing.T) {
	base := Default()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"合法多卷", func(c *Config) {
			c.Volumes = []VolumeConfig{{Name: "main", Root: "./storage", ACL: &VolumeACLConfig{Mode: "allow", Owners: []string{"alice"}}},
				{Name: "disk2", Root: "/mnt/disk2", VolCapacity: 1 << 30}}
		}, ""},
		{"非法 mode", func(c *Config) { c.Volumes[0].ACL = &VolumeACLConfig{Mode: "bogus"} }, "acl mode"},
		{"重复卷名", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "default", Root: "/x"})
		}, "重复"},
		{"非法卷名", func(c *Config) { c.Volumes[0].Name = ".." }, "非法"},
		{"负容量", func(c *Config) { c.Volumes[0].VolCapacity = -1 }, "不能为负"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := *base
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("期望通过, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}
```

- [ ] **步骤 2：运行验证失败**

运行：`go test -count=1 -run TestVolumesConfig ./pkg/server/` — 预期 FAIL（`VolumeConfig` 未定义、`Config.Volumes` 字段缺失）。

- [ ] **步骤 3：最小实现**

在 `config.go` 加类型与校验（字段放 Config 内 `OwnerQuotas`/`BucketLimits` 附近）：

```go
// VolumeACLMode 是卷 ACL 模式。allow=默认拒绝+白名单；deny=默认开放+黑名单。
type VolumeACLMode string

const (
	VolumeACLAllow VolumeACLMode = "allow"
	VolumeACLDeny  VolumeACLMode = "deny"
)

type VolumeACLConfig struct {
	Mode   VolumeACLMode `yaml:"mode" mapstructure:"mode"`
	Owners []string      `yaml:"owners" mapstructure:"owners"`
}

type VolumeConfig struct {
	Name        string            `yaml:"name" mapstructure:"name"`
	Root        string            `yaml:"root" mapstructure:"root"`
	VolCapacity int64             `yaml:"vol_capacity" mapstructure:"vol_capacity"`
	ACL         *VolumeACLConfig  `yaml:"acl,omitempty" mapstructure:"acl"`
}

// Config 顶层增字段：
// Placement 卷路由策略 prefer-default|spread（缺省 prefer-default）
Placement string `yaml:"placement" mapstructure:"placement"`
// Volumes 卷列表；缺省由 Default 烘焙单默认卷（name=default, root=defaultStorageRoot 占位，
// 装配层 F1 裁决为 cfg.StorageRoot）；零值 Config 由 SetDefaults/Validate 兜底合成。契约 Volumes 恒 ≥1。
Volumes  []VolumeConfig `yaml:"volumes" mapstructure:"volumes"`
```

`Default()` 里直接烘焙归一后的单卷形态：`Placement: "prefer-default"`、`Volumes: []VolumeConfig{{Name: "default", Root: defaultStorageRoot}}`（首卷 root 用占位常量 `defaultStorageRoot("./storage")`——注意不是 `c.StorageRoot`，故 YAML 只写 `storage_root:/data` 时 Default 的合成卷 root 停在占位，装配层以 `resolveDefaultVolumeRoot` 裁决为 `cfg.StorageRoot`，见任务 3 F1 门禁）。**契约「Volumes 恒 ≥1、首卷 = 默认卷」由 Default 预合成保证**；normalize 区（`:528` 附近 `c.StorageRoot` 兜底之后）仍保留 `len==0` 兜底（直接消费零值 Config 的路径同样得合成）加：

```go
if c.Placement == "" { c.Placement = "prefer-default" }
if len(c.Volumes) == 0 {
    c.Volumes = []VolumeConfig{{Name: "default", Root: c.StorageRoot}}
} else {
    for i := range c.Volumes {
        if c.Volumes[i].Root == "" {
            if i == 0 { c.Volumes[i].Root = c.StorageRoot } else { /* 保留空 → Validate 拒 */ }
        }
        if c.Volumes[i].ACL == nil {
            c.Volumes[i].ACL = &VolumeACLConfig{Mode: VolumeACLDeny} // 缺省开放
        }
        if c.Volumes[i].ACL.Mode == "" { c.Volumes[i].ACL.Mode = VolumeACLDeny }
    }
}
```

`Validate()` 加（`config.go` 顶部需 `import "github.com/cocomhub/sproxy/pkg/storage"`——包属 pkg/server 无环；复用现有 `storage.ValidSegmentName`）：

```go
switch c.Placement {
case "prefer-default", "spread":
default:
    return fmt.Errorf("placement 非法 %q：仅支持 prefer-default|spread", c.Placement)
}
seen := make(map[string]bool, len(c.Volumes))
for i := range c.Volumes {
    v := &c.Volumes[i]
    if !storage.ValidSegmentName(v.Name) {
        return fmt.Errorf("卷名 %q 非法（拒绝空/绝对/..、.__ 前缀、Windows 保留名与非法字符）", v.Name)
    }
    if seen[v.Name] { return fmt.Errorf("卷名重复 %q", v.Name) }
    seen[v.Name] = true
    if v.Root == "" { return fmt.Errorf("卷 %q root 为空（非首卷需显式指定挂载根）", v.Name) }
    if v.VolCapacity < 0 { return fmt.Errorf("卷 %q 容量上限 %d 非法：不能为负", v.Name, v.VolCapacity) }
    if a := v.ACL; a != nil {
        if a.Mode != VolumeACLAllow && a.Mode != VolumeACLDeny {
            return fmt.Errorf("卷 %q acl mode %q 非法：仅支持 allow|deny", v.Name, a.Mode)
        }
        for _, o := range a.Owners {
            if !storage.ValidSegmentName(o) { return fmt.Errorf("卷 %q acl owners 含非法 owner %q", v.Name, o) }
        }
    }
}
```

> 注意 `Validate` 目前收的是 `Default()` 后实例？——读 `Validate()` 调用方确认是否先 Normalize。若 `Validate` 在 Normalize 前被调（如测试直接 `&Config{...}.Validate()`），合成单卷逻辑放进 `Validate` 开头（幂等：仅 `len==0` 时合成），保证两种路径都得到校验。两种都实现：Normalize 合成 + Validate 兜底合成。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run TestVolumesConfig ./pkg/server/` — PASS。再 `go test -count=1 ./pkg/server/` 全量确认无回归（config 改动不应破坏既有）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/config.go pkg/server/config_volumes_test.go
git commit -m "feat(config): 卷配置模型 + ACL 校验——volumes/placement 键、缺省合成单卷、Validate 卷名校验"
```

---

### 任务 2：pkg/volume 纯域包（卷/ACL/选卷排序）

**文件：**
- 创建：`pkg/volume/volume.go`、`pkg/volume/volume_test.go`
- 修改：无（本任务不改 server）

**职责：** 无 I/O、无 quota、无 storage 依赖的纯逻辑：ACL 判定、允许卷集合、选卷排序（prefer-default / spread）。供 server 装配与 router 调用。**放 `pkg/volume`（非 internal）**，便于未来 cmd/ext 复用。

- [ ] **步骤 1：写失败测试** `pkg/volume/volume_test.go`

```go
package volume

import "testing"

func mustVol(name string) Volume { return Volume{Name: name} }

func TestAuthorize(t *testing.T) {
	denyOpen := Volume{Name: "a", ACL: ACL{Mode: ModeDeny, Owners: map[string]struct{}{"guest": {}}}}
	allowClosed := Volume{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}
	cases := []struct {
		name  string
		vol   Volume
		owner string
		want  bool
	}{
		{"deny 未在名单→开放", denyOpen, "alice", true},
		{"deny 命中黑名单→拒", denyOpen, "guest", false},
		{"allow 白名单命中→放行", allowClosed, "alice", true},
		{"allow 未命中→拒", allowClosed, "bob", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vol.Authorize(tc.owner); got != tc.want {
				t.Fatalf("Authorize(%q)=%v want %v", tc.owner, got, tc.want)
			}
		})
	}
}

func TestAllowedVolumes(t *testing.T) {
	vols := []Volume{
		{Name: "a", ACL: ACL{Mode: ModeDeny}},                                     // 开放
		{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}, // 仅 alice
		{Name: "c", ACL: ACL{Mode: ModeAllow}},                                   // allow 空=全拒
	}
	got := AllowedVolumes(vols, "bob")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("bob 应仅见卷 a, got %+v", names(got))
	}
}

func TestOrderCandidates(t *testing.T) {
	vols := []Volume{{Name: "main"}, {Name: "disk2", Capacity: 100}, {Name: "disk3", Capacity: 200}}
	// spread：按 (cap-used) 降序
	usage := map[string]int64{"disk2": 90, "disk3": 10}
	got := OrderCandidates(vols, ModeSpread, usage)
	if got[0].Name != "disk3" { t.Fatalf("spread 应选 disk3(余 190) 优先, got %+v", names(got)) }
	// prefer-default：main 恒最前，其余保持声明序
	got = OrderCandidates(vols, ModePreferDefault, usage)
	if got[0].Name != "main" { t.Fatalf("prefer-default 首卷恒优先, got %+v", names(got)) }
}
```

（`names` 为测试辅助返回 `[]string`。）

- [ ] **步骤 2：运行验证失败**

运行：`cd pkg/volume && go test -count=1 ./...` — FAIL（包/类型不存在）。

- [ ] **步骤 3：最小实现** `pkg/volume/volume.go`

```go
// Package volume 是 sproxy 多卷存储的纯域模型：卷定义、ACL 判定与选卷排序。
// 无 I/O、无 quota、无 storage 依赖——只做可测试的决策逻辑，装配/账本由 server 承担。
package volume

import "sort"

type Mode string

const (
	ModePreferDefault Mode = "prefer-default"
	ModeSpread        Mode = "spread"
	ModeDeny          Mode = "deny" // ACL 模式：默认开放 + 黑名单
	ModeAllow         Mode = "allow" // ACL 模式：默认拒绝 + 白名单
)

// ACL 是卷访问控制（装配期由 config 解析而来）。零值 = ModeDeny + 空名单（默认开放）。
type ACL struct {
	Mode   Mode
	Owners map[string]struct{}
}

// Volume 是装配后不可变卷描述。RootDir 由装配层持有根句柄，此处仅配置元数据。
type Volume struct {
	Name     string
	RootDir  string
	Capacity int64 // 0 = 不限制
	ACL      ACL
}

// Authorize 判定 owner 是否可用本卷。fail-closed：allow 未命中即拒。
func (v Volume) Authorize(owner string) bool {
	switch v.ACL.Mode {
	case ModeAllow:
		_, ok := v.ACL.Owners[owner]
		return ok
	default: // ModeDeny（含零值）：默认开放，黑名单命中才拒
		_, banned := v.ACL.Owners[owner]
		return !banned
	}
}

// AllowedVolumes 返回 owner 允许的卷子集（保持声明序）。
func AllowedVolumes(vols []Volume, owner string) []Volume {
	out := make([]Volume, 0, len(vols))
	for _, v := range vols {
		if v.Authorize(owner) {
			out = append(out, v)
		}
	}
	return out
}

// DefaultVolume 返回默认卷（首个）。空列表返回零值 Volume{Name:"<none>"}。
func DefaultVolume(vols []Volume) Volume {
	if len(vols) == 0 { return Volume{Name: "<none>"} }
	return vols[0]
}

// OrderCandidates 依 placement 策略对候选卷排序（均已过 ACL）：
//   - prefer-default：首个（默认卷）最前，其余保持声明序（默认卷满才轮询后续）；
//   - spread：按 (Capacity-used) 余量降序（used 缺省 0；Capacity 0 视为余量无限）。
func OrderCandidates(vols []Volume, placement Mode, used func(name string) int64) []Volume {
	out := append([]Volume(nil), vols...)
	switch placement {
	case ModeSpread:
		sort.SliceStable(out, func(i, j int) bool {
			return remain(out[i], used) > remain(out[j], used)
		})
	default: // prefer-default：声明序即默认卷优先（AllAllowed 保持声明序）
	}
	return out
}

func remain(v Volume, used func(name string) int64) int64 {
	u := int64(0)
	if used != nil { u = used(v.Name) }
	if v.Capacity <= 0 { return int64(1) << 62 } // 不限容量视为巨量余量
	if u >= v.Capacity { return 0 }
	return v.Capacity - u
}
```

> 说明：`Volume.RootDir` 任务 2 未消费（供装配层把根句柄与配置关联），测试暂不覆盖。`OrderCandidates` 用 `used` 回调保持包纯净（server 装配传卷池 Usage 闭包）。

- [ ] **步骤 4：运行验证通过**

运行：`cd pkg/volume && go test -count=1 ./...` — PASS。`go vet ./pkg/volume`。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/volume.go pkg/volume/volume_test.go
git commit -m "feat(volume): 纯域包——卷/ACL 判定/允许集合/选卷排序（prefer-default|spread）"
```

---

### 任务 3：装配改造——多卷根 + owner 卷视图 + 卷容量池 + reconcile 双目标（单卷零回归）

**文件：**
- 修改：`pkg/server/handlers.go`（容器 :96-106、装配 :560-700、`tenantFor` :218 后新增辅助）
- 修改：`pkg/server/quota_reconcile.go`（reconcile 逐卷双校准）
- 创建：`pkg/server/volumes.go`（server 侧 VolumeSet 装配 + 视图/定位辅助）、`pkg/server/volumes_test.go`（单卷退化 + 多卷根装配）
- 修改：`cmd/sproxy/root.go`（无——装配在 RegisterRoutes 内；仅确认 cfg 传入路径）

**职责：** 多卷根打开 + 容器扩展 + owner 卷视图（懒缓存）+ 每卷容量 Pool + reconcile 目标含卷容量。**本任务后既有 handler 仍只走默认卷**（T4 起卷感知），因此零回归可测。

- [ ] **步骤 1：读现状** — 通读 `pkg/server/handlers.go:560-701` 与 `pkg/server/quota_reconcile.go`，确认 `RegisterRoutes`/`NewHandlers` 装配函数名与 `ReconcileFunc` 数据流。

- [ ] **步骤 2：写失败测试** `pkg/server/volumes_test.go`

```go
func TestAssembly_SingleVolumeDegrades(t *testing.T) {
	// 未配 volumes：RegisterRoutes 单卷（name=default, root=StorageRoot）→ globalRoot 语义不变。
	// 用既有 newTestServer（integration_test.go）起服务，upload+list+download 全通（零回归由
	// 该测试 + 既有 integration 全量共同证明；此处重点断言装配产物）。
	srv := newTestServer(t) // 若签名不符，读 integration_test.go newTestServer 实际签名调整
	// 请求 /api/volumes 需登录/签名——集成环境凭据见 newTestServer 用法；此处用不签名 list 或
	// 直接测内层：newTestServer 若暴露 Handlers 则查 h 的卷容器字段长度==1。
}

func TestAssembly_MultiVolumeRoots(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()} // 两块假盘
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0]},
		{Name: "disk2", Root: dirs[1], VolCapacity: 100},
	}
	if err := cfg.Validate(); err != nil { t.Fatal(err) }
	// 装配（复用 RegisterRoutes 或抽出的 assembleVolumes）后：
	//   - volumeRoots 含 main/disk2 两卷，各 LAYOUT_VERSION 就位
	//   - volumePools["disk2"].MaxBytes()==100
	//   - anonymous 预创建在 main（默认卷）根
}
```

> 集成断言形态取决于 `newTestServer` 暴露面。若无法经 HTTP 断言内部，改用「可抽出的装配函数」单测：计划鼓励把「打开 N 卷 + 建池」抽成可独立测试的 `func assembleVolumes(cfg *Config, log *slog.Logger) (*volumeSet, error)`（放 `volumes.go`），测试直接调它断言。

- [ ] **步骤 3：最小实现**

`pkg/server/volumes.go`（核心新结构）：

```go
// volumeSet 是装配后的卷集合：配置 + 每卷打开的根 + 每卷容量池 + ACL。
// 默认卷 = cfg.Volumes[0]；globalRoot/globalPool 语义映射到默认卷，保持既有 handler 不改。
type volumeSet struct {
	volumes    []volume.Volume        // 装配后不可变卷描述（含默认卷在 [0]）
	roots      map[string]*storage.Root // name → 打开的根（含默认卷）
	pools      map[string]*quota.Pool   // name → 卷容量池（Capacity<=0 仍建池不限量，便于统一入账）
	defaultName string
}

func assembleVolumes(cfg *Config, log *slog.Logger) (*volumeSet, error) {
	vs := &volumeSet{roots: map[string]*storage.Root{}, pools: map[string]*quota.Pool{}}
	for i, vc := range cfg.Volumes {
		if err := os.MkdirAll(vc.Root, 0o755); err != nil { return nil, fmt.Errorf(...) }
		rt, err := storage.OpenRoot(vc.Root)
		if err != nil { return nil, fmt.Errorf("打开卷 %q 根失败: %w", vc.Name, err) }
		vs.roots[vc.Name] = rt
		vs.pools[vc.Name] = quota.NewPool(vc.VolCapacity)
		if i == 0 {
			vs.defaultVol = rt
			vs.defaultName = vc.Name
		}
		// ACL 解析成 map
	}
	return vs, nil
}
```

`handlers.go` 容器扩展（保持旧字段存在）：

```go
volSet   *volumeSet                        // 装配后的卷集合（nil = 未装配卷功能，旧装配路径）
// tenantViews map[string]map[string]*storage.Tenant  // owner → 卷名 → Tenant（懒；默认卷租户即现 tenantRoots[owner]）
```

`tenantFor(owner)` 保持返回**默认卷**租户（不动，globalRoot 字段 = 默认卷 root，见装配接线）：
- 装配：`globalRoot = vs.defaultVol`（若 vs != nil），`globalPool = quota.NewPool(cfg.MaxStorageBytes)`（owner 全局 max 兜底不变），新增 `h.volSet = vs`。
- 预创建 anonymous 走默认卷（现有 `tenantFor` 路径，不改）。

`listTenantIDs`（cloud 恢复扫描用）现扫 `globalRoot`（默认卷）。cloud 落默认卷（规格 AD-5）→ 语义仍对。多卷其它卷上的 owner 是否被 cloud 恢复扫描遗漏？cloud 仅默认卷 → 不受影响。记录不改。

reconcile 双目标：`reconcileQuotaScopes` 现收 `tenantBuckets map[tenant]map[bucket]int64`（单根扫描产物）。多卷扫描需逐卷。改法（本任务实现，T4 后真正多卷扫描才有意义，但先立框架）：

```go
// 新增：多卷逐卷扫描入口（装配为 ReconcileFunc 时以卷维度展开）
func (h *Handlers) reconcileVolumes(volumeBuckets map[string]map[string]map[string]int64) {
	// volumeBuckets[卷名][tenant][bucket]=bytes：逐卷
	//   1) 逐卷调用现有校准逻辑对 owner 全局 Scope（注意：owner Scope 在全局池，跨卷合计——
	//      校准目标 = 该 owner 全卷合计磁盘占用。因此不能简单逐卷 Adjust 同一 Scope 而重复，
	//      需先汇总所有卷的 tenantBuckets → 现有 reconcileQuotaScopes 单次执行。）
	//   2) 逐卷：pool := h.volSet.pools[name]；重算卷 committed = Σ 该卷所有 tenantBuckets 桶字节，
	//      pool.Adjust 收敛。
}
```

> **校准聚合语义**：owner 全局 Scope 的磁盘实际 = 该 owner 在**所有允许卷**占用之和。多卷模式下扫描先把各卷按 owner 汇总成一份 `tenantBuckets`（现有逻辑不变），再单独做每卷容量池校准。ReconcileFunc 签名若不变（`map[tenant]map[bucket]int64`），则逐卷扫在 **StorageManager/扫描层**（`storage_manager.go`）产生聚合输入给现有 reconcile，另把 per-卷归集喂卷池校准——实现需读 `storage_manager.go` 的 `ScanAndRecalculate` 归集逻辑后接线（本任务至少让「多卷根 + 池」就位，reconcile 多卷逐卷校准留 T4 后与写路径一并验证；但**框架与单卷不回归**必须先绿）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 ./pkg/server/...` 全绿（单卷零回归主证据）。再 `go test -race -count=1 -run TestAssembly ./pkg/server/`。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/volumes.go pkg/server/volumes_test.go pkg/server/handlers.go pkg/server/quota_reconcile.go
git commit -m "feat(server): 卷集合装配——多卷根 + 每卷容量池 + 默认卷语义映射（单卷零回归）"
```

---

### 任务 4：upload 卷路由（选卷 + 双账本 + ACL + 显式 volume + 唯一性）

**文件：**
- 修改：`pkg/server/upload_handler.go`（`upload` 主流程 + 新路由函数）
- 修改：`pkg/server/volumes.go`（加 `routeUpload` 选卷 + 双 reserve）
- 测试：`pkg/server/upload_volume_test.go`（新建，多卷假盘集成）

**职责：** 写路径真正卷感知：候选序（默认卷优先/可配 spread）→ ACL 过滤 → owner 全局 + 卷容量双 TryReserve → 落默认/换卷 → 唯一性（显式 volume 时）。checksum 仍记默认卷（meta 归属，不改）。

- [ ] **步骤 1：读现状** — 精读 `pkg/server/upload_handler.go` 的 `upload` 主体：现走 `h.tenantOf(r)` → `tenant.UserRel` → `quotaScopeFor(owner, rel)` TryReserve → 写盘 → Commit。确认 reserve 句柄与写盘函数边界（能否整体替换为「选卷 + 目标 Tenant/Scope」）。

- [ ] **步骤 2：写失败测试** `pkg/server/upload_volume_test.go`

```go
// 双卷（main 默认容量小、disk2 大），prefer-default：main 未满落 main，main 满换 disk2。
func TestUpload_RoutesToDefaultThenNext(t *testing.T) {
	// 用 integration 基建（newTestServer / newTestServerWithAllRoutes）起双卷 server：
	// cfg.Volumes = [{main, 10B}, {disk2, 1MiB}]，placement=prefer-default。
	// upload 8B → 200，响应/内部断言落 main；
	// 再 upload 8B → 200 落 disk2（main 满换卷）；main 下 1 文件、disk2 下 1 文件。
	// 断言方式：经 sclient/HTTP 不直接暴露卷时，用 t.TempDir 假盘直接查磁盘：
	//   main 根 user/<owner>/<a> 存在、disk2 根 user/<owner>/<b> 存在。
}

func TestUpload_ExplicitVolume(t *testing.T) {
	// upload 带 volume=disk2 表单字段 → 200 落 disk2（main 未满也指定生效）。
	// volume=ghost（不在视图）→ 403 volume not allowed。
	// volume=disk2 且该 rel 已在 main → 409 volume conflict（唯一性）。
}

func TestUpload_OwnerGlobalQuotaCrossVolume(t *testing.T) {
	// owner_quotas[owner]=15B，双卷各 1MiB。两次 8B 落不同卷，第三次 8B → ErrStorageFull（owner 全局满，
	// 不因有第二卷而放行）。
}
```

> 这些测试驱动「卷可见」断言。若既有测试基建把卷名暴露进响应头最省事（见实现：upload 成功响应头可加 `X-Volume`，helper 易断言），implementer 可选加该响应头并在测试用它断言落卷。

- [ ] **步骤 3：最小实现**

`pkg/server/volumes.go` 加路由：

```go
// routeUpload 为 owner 的 rel 选目标卷并在 owner 全局 + 卷容量双账本预留。
// 返回 (卷名, *storage.Tenant, 该卷 quota Scope/预留句柄)。placement/ACL/换卷全在此。
// 语义：
//   - 候选 = AllowedVolumes(owner 视图)；
//   - 显式 volume（非空）→ 仅该卷候选，且须在视图（不在视图 = 403）；
//   - 候选按 placement 排序后依序尝试：owner 全局池 TryReserve 成功 → 卷池 TryReserve：
//       卷池满 → Release owner 预留 → 下一候选；owner 全局满 → 直接 ErrStorageFull 不换卷；
//   - 显式 volume 时先查唯一性（目标卷已有同 rel → 409；其它卷已有同 rel → 409+提示卷名）。
func (h *Handlers) routeUpload(owner, rel, explicitVol string) (string, *storage.Tenant, error)
```

`upload_handler.go` `upload` 改动：
1. 取 owner、解析 multipart 后得到 `remotePath` → `NormalizeRemote` → rel（现 user/ 前缀映射逻辑）。显式卷从表单/query `volume` 读（`r.FormValue("volume")`）。
2. 替换「单卷 tenantOf + quotaScopeFor TryReserve」为 `routeUpload` 返回的目标卷 tenant + 预留。
3. 写盘用目标卷 tenant 的 Root（不再恒 `h.tenantOf(r)`）。
4. 成功响应头加 `X-Volume`（helper 断言用；向后兼容：多余响应头无破坏）。checksum 记默认卷 store（`checksumStoreFor(owner)` 不变——其内部走默认卷 `tenantFor`，meta 归属自动满足）。
5. 双 Commit：owner Scope + 卷池（routeUpload 返回两者句柄；或卷池 reserve 由 routeUpload 内部持句柄，upload 结束统一 commit——随现 reserve 句柄形态定，须保持「写失败 → 双 release」）。

> **唯一性快路径**：自动路由下 rel 由路由唯一决定，无需跨卷查重；仅显式指定时查（目标卷 stat + 视图其余卷 stat）。本任务实现该分支。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run 'TestUpload_(RoutesToDefaultThenNext|ExplicitVolume|OwnerGlobalQuotaCrossVolume)' ./pkg/server/` — PASS。再全量 `go test -count=1 ./pkg/server/...`（零回归）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/upload_handler.go pkg/server/volumes.go pkg/server/upload_volume_test.go
git commit -m "feat(server): upload 卷路由——placement 选卷 + 双账本预留 + 显式 volume + 唯一性查重 + X-Volume 响应头"
```

---

### 任务 5：读路径卷定位 + 显式 volume 过滤（download/stat/list/delete/rename/目录）

**文件：**
- 修改：`pkg/server/volumes.go`（`locate` 辅助：owner 视图内定位 rel 所在卷/tenant）
- 修改：`pkg/server/download_handler.go`、`list_handler.go`、`delete_handler.go`、`rename_handler.go`、`dirs.go`
- 测试：`pkg/server/locate_volume_test.go`（新建）

**职责：** 读/删/改名跨卷定位：先默认卷快路径（`tenantFor` 命中即用，保持既有 handler 主路径几乎不变），未命中才遍历视图其余卷。list 聚合多卷 + `volume` 字段 + `?volume=` 过滤。

- [ ] **步骤 1：写失败测试** `pkg/server/locate_volume_test.go`

```go
// download：文件在 disk2（非默认卷）也能定位下载（默认卷先 miss → 视图遍历命中 disk2）。
func TestDownload_LocatesAcrossVolumes(t *testing.T) {
	// 双卷 server。upload 塞满 main 使第二个文件落 disk2。
	// download disk2 上文件 → 200 且内容一致。
}

// download 默认卷快路径不受影响 + 全视图 miss → 404。
func TestDownload_MissAllVolumes404(t *testing.T) { /* download 不存在文件 → 404 */ }

// list 聚合两卷：两卷各一文件 → GET /api/files 返回两条，各带 volume 字段区分。
// ?volume=main 只返回 main 那条。
func TestList_AggregatesVolumesAndFilters(t *testing.T) {
	// list 返回 JSON files[] 每项含 volume；断言聚合数与过滤数。
}

// delete/rename 定位跨卷：删 disk2 文件 → 200 且 disk2 下消失、main 不受影响。
// rename 同卷（不跨卷）→ 200。
func TestDeleteRename_LocatesAcrossVolumes(t *testing.T) {}

// stat（HEAD /api/files/stat）定位跨卷。
func TestStat_LocatesAcrossVolumes(t *testing.T) {}
```

- [ ] **步骤 2：运行确认失败** — `go test -count=1 -run TestDownload_LocatesAcrossVolumes ./pkg/server/` 等 FAIL。

- [ ] **步骤 3：最小实现**

`pkg/server/volumes.go` 加定位：

```go
// locateOwnerFile 返回 owner 视图内 rel（user/ 前缀完整桶路径）所在卷的 *storage.Tenant。
// 默认卷优先（快路径，覆盖绝大多数既有行为）；未命中遍历视图其余卷。
// 全部未命中返回 (nil,false)。唯一性保证下不会出现多卷命中；防御性若命中>1 返回首卷（注释注明）。
func (h *Handlers) locateOwnerFile(owner, userRel string) (*volumeView, bool)
```

各 handler 改法（**模式统一**，逐个文件替换「`h.tenantOf(r)` 直接操作」为「`locateOwnerFile` → 命中才操作，未命中 404」；list 特殊）：
- `download_handler.go`：`h.tenantOf(r)` → 定位；Range/If-Range 逻辑不动，仅目标 tenant 换。
- `list_handler.go`：聚合循环 owner 视图每卷 `ReadDir`（每卷返回项合并；条目带 `volume`）；`?volume=` 存在时只列该卷（且先 ACL 校验）。checksum/大小字段每卷各自读。
- `delete_handler.go` / `rename_handler.go`：定位到卷后操作；rename 保持同卷（跨卷移动走 T6 move API）。
- `dirs.go`（mkdir/rmdir）：mkdir 落默认卷（新建空目录；跨卷建目录语义留 T6 定——mkdir 无文件定位，默认落默认卷，保持简单并在注释说明）；rmdir 定位删除。
- `checksumStoreFor`/checksum 校验不动（默认卷单一，rel 全局唯一即可查）。
- 显式 `volume` 参数（query `?volume=`）：download/delete/stat/rename 若带则只在指定卷定位（先 ACL）；不在视图 → 403/404。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run 'Test(Download|List|Delete|Rename|Stat)_LocatesAcrossVolumes|TestList_AggregatesVolumesAndFilters|TestDownload_MissAllVolumes404' ./pkg/server/` — PASS；全量 `./pkg/server/...` 零回归。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/volumes.go pkg/server/download_handler.go pkg/server/list_handler.go pkg/server/delete_handler.go pkg/server/rename_handler.go pkg/server/dirs.go pkg/server/locate_volume_test.go
git commit -m "feat(server): 读路径跨卷定位——download/stat/list/delete/rename 视图遍历 + volume 过滤 + list 聚合带卷"
```

---

### 任务 6：特征桶跟随 + meta 归属 + 新 API（/api/volumes、/api/volumes/move）

**文件：**
- 修改：`pkg/server/chunked_upload.go`（`/upload/init` 定卷 + `complete` 同卷）、`pkg/server/chunked_download.go`（下载定位随卷）
- 修改：`pkg/server/cloud_download.go`、`cloud_download_handler.go`、`archive.go`（cloud/archive 产物绑默认卷）
- 修改：`pkg/server/versioning` 相关（version 桶随 user 文件卷）
- 创建：`pkg/server/volumes_api.go`（`GET /api/volumes`、`POST /api/volumes/move`）+ 路由注册（handlers.go localMux/srvMux）
- 测试：`pkg/server/feature_bucket_volume_test.go`、`pkg/server/volumes_api_test.go`

**职责：** chunk init 即定卷（保证 complete 同卷原子）；version 随 user 卷；cloud/archive 产物落默认卷（例外明确）；checksum/凭据/状态确认默认卷不变式（补防御性测试）；新 API。

- [ ] **步骤 1：写失败测试**

```go
// 分块上传：init 定卷 → 每 chunk 存该卷 chunk/<id>/ → complete 后 user 文件在该卷（非默认也成立）。
func TestChunkedUpload_InitPinsVolume(t *testing.T) {
	// 塞满默认卷后 init+chunk+complete 一个文件 → 断言完整文件落 disk2（init 阶段路由选卷）。
	// 关键：init 阶段即做路由（预留 disk2），chunk 写 disk2，complete 同卷 rename 成功。
}

// version：默认卷文件存版本同卷；disk2 文件存版本在 disk2 version/。
func TestVersioning_FollowsUserVolume(t *testing.T) {}

// cloud 下载产物落默认卷 cloud/（即便磁盘在主卷）。archive 产物落默认卷 archive/。
func TestCloudArchive_DefaultVolume(t *testing.T) {}

// meta 不变式：credentials.json/checksums.json 只在默认卷 <owner>/meta/，非默认卷无。
func TestMeta_DefaultVolumeOnly(t *testing.T) {
	// 多卷写文件后断言默认卷 meta/checksums.json 存在且记录 rel；disk2 <owner>/meta 下无 checksums.json。
}

// 新 API：
// GET /api/volumes → owner 可见卷 [{name, mode, capacity, usage, allowed}]；usage=卷池 Usage。
// POST /api/volumes/move?from_volume=main&to_volume=disk2&filename=x → x 迁到 disk2 同 rel；
//   目标卷已有同 rel → 409；from/to 不在视图 → 403/404；目标卷容量不足 → ErrStorageFull。
func TestVolumesAPI_ListAndMove(t *testing.T) {}
```

- [ ] **步骤 2：运行确认失败**

- [ ] **步骤 3：最小实现**

- `chunked_upload.go` `init`：调 `routeUpload(owner, rel, "")`（默认路由）**定卷**并把卷名存会话元数据（UploadStore 会话目录加卷字段或派生到目标卷 chunk 桶）；`chunk`/`status` 从会话取卷；`complete` 同卷把分块合入目标卷 `user/`。分块上传存储对象目前按 owner 一个 UploadStore 指向默认卷 `chunk` 桶——需改为**每 (owner,卷) 会话目录或会话记录卷名**。读 `chunked_upload.go`/`uploadStoreFor` 后定最小改法（倾向：UploadStore 结构加会话→卷映射，chunk 桶仍在各卷自身 `chunk/<owner>/` 还是统一默认卷？——**必须 chunk 与目标 user 同卷**保证 rename 原子，故 chunk 桶 = 目标卷内）。
- version：`versioning` 保存/恢复定位文件所在卷（用 T5 `locateOwnerFile`）后在该卷 version 桶操作。
- cloud：`NewCloudDownloadManager(..., cfg.StorageRoot, ...)` 现绑单根 → 改绑**默认卷 root**（`cloud_download.go` 的存储根参数传默认卷；云任务元数据/产物全默认卷）。
- archive：产物 `archive/<name>.tar.gz` 落默认卷；输入读取经定位。
- `volumes_api.go`：`GET /api/volumes` 返回 owner 可见卷（`AllowedVolumes` + 每卷 `pool.Usage()`/`Capacity` + ACL mode）；`POST /api/volumes/move`：from/to 同 owner 视图校验 → 源在 from 卷（`locate` 定位到 from）→ 目标 to 卷唯一性查重 → to 卷池 + owner 全局 reserve → 流式复制到 to 卷临时文件 → fsync → 原子 rename → 删源 → 双 release。源在 from 卷但 to==from → 直接同卷 rename。
- 路由注册：`GET /api/volumes`、`POST /api/volumes/move` 进 srvMux（authMiddleware）；POST 仅 localMux?（与 upload/delete 一致——现 upload 在 localMux + srvMux 双注册？读 handlers.go:704-709 及 srvMux 段后按同款注册）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run 'Test(ChunkedUpload_InitPinsVolume|Versioning_FollowsUserVolume|CloudArchive_DefaultVolume|Meta_DefaultVolumeOnly|VolumesAPI_)' ./pkg/server/` — PASS；全量零回归。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/chunked_upload.go pkg/server/chunked_download.go pkg/server/cloud_download.go pkg/server/cloud_download_handler.go pkg/server/archive.go pkg/server/volumes_api.go pkg/server/handlers.go pkg/server/feature_bucket_volume_test.go pkg/server/volumes_api_test.go
git commit -m "feat(server): 特征桶跟随 user 卷 + 默认卷 meta 归属 + /api/volumes 与跨卷 move"
```

---

### 任务 7：客户端表面——FileClient 卷上下文 + sclient volumes/--volume + WebUI

**文件：**
- 修改：`pkg/client/client.go`（`FileClient` 增 `Volume` 可选上下文；List 返回条目带 Volume）、`pkg/client/dirs.go`、`pkg/client/download_items.go`、`pkg/client/stream.go`/`chunked.go`（upload/download 透传 volume 参数）
- 修改：`cmd/sclient/upload.go`、`list.go`、`mv.go`、`stat.go`/`download.go`（`--volume`）、`root.go`（注册 `volumes` 子命令）；新建 `cmd/sclient/volumes.go`（`volumes` 列表 + `mv --to-volume` 内部调 move API）
- 修改：`web/static/`（index.html / app.js / app-render.js / style.css：list 行卷 badge + 卷仪表 + 上传下拉）
- 测试：`pkg/client/volume_test.go`、`cmd/sclient/volumes_cmd_test.go`、WebUI `node --check` + 既有 web-test

**职责：** 默认无感（不加参数走 auto）；深入能力显式加。向后兼容。

- [ ] **步骤 1：读现状** — `pkg/client/client.go` 的 `FileClient` 结构与方法签名（`Upload`/`List`/`Download`/`Stat`/`Delete`），`cmd/sclient/*.go` 命令如何持 `*client.FileClient`，`web/static` list 渲染入口。

- [ ] **步骤 2：写失败测试** `pkg/client/volume_test.go`

```go
// FileClient.Upload 带 Volume="disk2" → 请求带 volume=disk2 参数（mock/真实 server 断言）。
// FileClient.List 返回条目带 Volume 字段（与 server 响应 volume 对齐）。
func TestClient_VolumeRoundtrip(t *testing.T) {
	// 用 pkg/testutil/mockserver 或真实 newTestServer 起双卷；client.Upload(Volume:"disk2")
	// → 落 disk2；client.List() 含该条目且 Volume=="disk2"。
}
```

sclient `volumes` 子命令与 `--volume` flag 测试（cmd 测试遵循既有 capture 模式 + config 隔离约束）。

- [ ] **步骤 3：最小实现**

- FileClient：请求构造处把 `Volume`（若非空）作为 `volume` 参数附加（upload 表单字段 / download·list·stat·delete query）。List 响应解析把服务端 `volume` 字段填入条目结构。
- sclient：`upload.go`/`list.go`/`download.go`/`stat.go`/`mv.go`/`delete.go` 加 `--volume` flag（可选，默认空 = auto）；`volumes.go` 新增 `volumes` 子命令（GET /api/volumes 展示 name/mode/capacity/usage/allowed，人类可读）；`mv` 加 `--to-volume`：目标卷与源不同 → 调 move API；同卷省略仍走 rename。
- WebUI：list 渲染行加卷 badge（条目 `volume` 字段）；顶栏或独立面板调 `/api/volumes` 显示卷仪表；上传表单加「卷」下拉（auto + 可见卷），仅当 volume 字段存在时发给服务端。
- 文档：`config.example.yaml`、`docs/config.md` 增 volumes/placement/vol_capacity/acl 示例与说明（SPDX + UTF-8）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 ./pkg/client/...`、`go test -count=1 ./cmd/sclient/...`、`make web-test`、受影响包 `-race`。全仓 `make test-all`/`make lint` 全绿。curl 实证：双卷配置起 sproxy → upload/list/download/volumes/move 真实 HTTP 验证 + 单卷配置回归。

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/ cmd/sclient/ web/static/ config.example.yaml docs/config.md
git commit -m "feat(client): 卷上下文透传 + sclient volumes/--volume/mv --to-volume + WebUI 卷 badge 与仪表"
```

---

## 收尾（全部任务后）

1. **整分支最终审查**（SDD：最强模型 code-reviewer，覆盖 merge-base..HEAD）。
2. **端到端实证清单**：单卷（缺省 config）全量回归 curl；双卷 config upload（auto 落 main → 满换 disk2）→ list 聚合带卷 → download 跨卷定位 → 显式 `--volume` → ACL 403 → move 跨卷 → 卷仪表。
3. **文档**：更新 `CLAUDE.md` 路由表（`/api/volumes`、`/api/volumes/move`、upload/download `volume` 参数）与「关键路由」章节；`docs/architecture.md` 多卷布局一节。
4. Push 分支 → PR → CI（ubuntu+windows test、lint、build-all）→ 用户人工 squash 合并 → 清分支 → 更新 memory（X 完成 + Y 前向）。

## 自检记录（writing-plans 自检）

- **规格覆盖**：§2 DoD 1-6 → T1/T3（单卷退化）、T4（路由/双账本/唯一）、T2/T3（ACL/装配）、T6（ACL 矩阵经 API/元）、T7（客户端+文档）覆盖。AD-1..AD-9 → T1(config)/T2(域)/T3(根+池)/T4(路由+双账)/T5(定位)/T6(特征桶+meta+API)/T7(表面)。§17 三缝 → T2（卷域包纯接口）+ T3（装配薄层）+ pkg/store 沿用不新增（无专门任务，规格明示不新建一致性抽象）。
- **占位符**：无「待定/TODO」；StorageManager/cloud 版本锚点、`newTestServer` 签名等以「读后定」标注但给出确定方向，不留空洞步骤。
- **类型一致**：`VolumeConfig/VolumeACLConfig/volume.Volume/ACL/Mode` 跨 T1-T3 一致；`placement` 值 `prefer-default|spread` 一致；错误码 403/409/404 一致；`GET /api/volumes`、`POST /api/volumes/move` 端点 T6/T7 一致；`X-Volume` 响应头 T4 定义、T7 client 消费。
