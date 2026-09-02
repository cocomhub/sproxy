# 多租户存储布局重构实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 废弃 `.__xx__` 魔法内部目录，落地租户自包含存储布局（`<tenant>/{user,cloud,archive,chunk,version,meta}/`），引入 `pkg/store`（记录存储）、`pkg/storage`（os.Root 布局）、`pkg/quota`（Pool/Scope 配额），按租户独立配额 + 全局兜底，严格租户隔离。

**架构：** 三个新一级包（`store`/`storage`/`quota`）作为叶子领域包；`pkg/server` 与 `pkg/server/syncmgr` 依赖它们。每个租户一个 `*os.Root`（防穿越由标准库保证）+ 一个 `quota.Scope`（父子链自动向全局聚合）。用户文件 rel 相对 `user/` 根，功能桶（`cloud/archive/chunk/version/meta`）与用户命名空间物理隔离。P0-P5 阶段：新包先行、测试全绿后逐步迁移旧 handler、P5 删除旧实现。

**技术栈：** Go 1.26 标准库（`os.Root`、`encoding/json`、`sync`），无新增第三方依赖（严格 stdlib 政策）。

**权威规格：** `docs/superpowers/specs/2026-09-01-multitenant-storage-layout-design.md`（目录布局、包结构、配额操作集、功能映射表、边界清单以此为准，本计划与规格冲突时以规格为准）。

---

## 执行总则

- **每个任务 = 一个可独立测试的交付物**，由子 agent 实现，完成后由独立审查 agent 审查（正确性/完整性/可用性/无缺陷），对照设计文档第 9 节边界清单核对。
- **无文件交集的任务可并行**；同一文件不得并行修改。
- **TDD**：每任务按"写失败测试 → 跑失败 → 实现 → 跑通过 → commit"推进。
- **测试命令**（sproxy 根）：`go test -count=1 ./pkg/...`；子 module 需 `cd` 进入对应目录（`pkg/tunnel/xfer/ext/*` 本次不动）。
- **提交规范**：Conventional Commits 中文，如 `feat(quota): Pool/Scope 配额池`、`refactor(server): upload 链路迁移到 Tenant API`。
- **阶段门禁**：每阶段所有任务实现 + 独立审查通过 + 对应测试全绿才进入下一阶段。

---

## 阶段 P0：三个新包（纯新代码，无既有代码改动）

### 任务 1：`pkg/quota` — 通用配额池（Pool/Scope/Reservation）

**目标：** 实现通用配额领域：路径化子池叠加、预留句柄对账、释放/调整、占用查询，不引入租户概念。

**文件：**
- 创建：`pkg/quota/quota.go`
- 测试：`pkg/quota/quota_test.go`

- [ ] **步骤 1：编写失败测试**（覆盖操作集全生命周期）

```go
func TestScope_TryReserveCommitRelease(t *testing.T) {
    root := NewPool(100)
    s := root.Scope("/t/alice", 50)
    res, err := s.TryReserve(30)          // 预留 30
    if err != nil { t.Fatalf("reserve: %v", err) }
    if got := s.Reserved(); got != 30 { t.Fatalf("Reserved=%d want 30", got) }
    res.Commit(25)                         // 实际 25：reserved 25→committed 25，多预留 5 归还
    if got := s.Usage(); got != 25 { t.Fatalf("Usage=%d want 25", got) }
    if got := s.Reserved(); got != 0 { t.Fatalf("Reserved=%d want 0", got) }
    if got := root.Usage(); got != 25 { t.Fatalf("root Usage=%d want 25（父链聚合）", got) }
    s.ReleaseUsage(10)                     // 删除文件释放 10
    if got := s.Usage(); got != 15 { t.Fatalf("Usage=%d want 15", got) }
}

func TestScope_Adjust(t *testing.T) {
    root := NewPool(100)
    s := root.Scope("/t", 100)
    s.Adjust(0, 10)                         // 建立占用 10（diff 语义：committed += next-prev）
    if got := s.Usage(); got != 10 { t.Fatalf("Usage=%d want 10", got) }
    s.Adjust(10, 20)                        // 覆盖写尺寸 10→20，diff +10 → 20
    if got := s.Usage(); got != 20 { t.Fatalf("Usage=%d want 20", got) }
    s.Adjust(20, 5)                         // 缩小 diff -15 → 5
    if got := s.Usage(); got != 5 { t.Fatalf("Usage=%d want 5", got) }
}

func TestScope_QuotaExceeded(t *testing.T) {
    root := NewPool(10)                     // 全局兜底 10
    s := root.Scope("/t", 8)                // 租户上限 8
    if _, err := s.TryReserve(9); !errors.Is(err, ErrStorageFull) {
        t.Fatalf("租户上限应拒绝, got %v", err)
    }
    if got := s.Available(); got != 8 { t.Fatalf("Available=%d want 8", got) }
}

// 全局兜底：两个租户各自未超自身上限，但总和超全局上限 → 必须拒绝（验证父链聚合检查）。
func TestScope_GlobalCapExceeded(t *testing.T) {
    root := NewPool(10)
    a := root.Scope("/a", 100)
    b := root.Scope("/b", 100)
    resA, err := a.TryReserve(6)            // 全局 6/10
    if err != nil { t.Fatalf("a 预留失败: %v", err) }
    if _, err := b.TryReserve(6); !errors.Is(err, ErrStorageFull) {
        t.Fatalf("全局兜底应拒绝（6+6>10）, got %v", err)
    }
    resA.Release()                          // 归还
    if _, err := b.TryReserve(6); err != nil {
        t.Fatalf("释放后应可预留, got %v", err)
    }
}

func TestScope_ReleaseReservation(t *testing.T) {
    root := NewPool(100)
    s := root.Scope("/t", 50)
    res, _ := s.TryReserve(40)
    res.Release()                            // 失败取消预留
    if got := s.Reserved(); got != 0 { t.Fatalf("Reserved=%d want 0", got) }
    if got := s.Available(); got != 50 { t.Fatalf("Available=%d want 50", got) }
}

func TestScope_UsageByBucket(t *testing.T) {
    root := NewPool(1000)
    t1 := root.Scope("/tenant/a", 500)
    t1.Scope("/user").Adjust(0, 100)         // 子桶 user 占用 100
    t1.Scope("/cloud").Adjust(0, 50)         // 子桶 cloud 占用 50
    m := root.UsageByBucket()
    if m["/tenant/a/user"] != 100 || m["/tenant/a/cloud"] != 50 {
        t.Fatalf("UsageByBucket=%v", m)
    }
    if got := t1.Usage(); got != 150 { t.Fatalf("tenant Usage=%d want 150", got) }
}
```

- [ ] **步骤 2：运行测试确认失败**

运行：`go test -count=1 ./pkg/quota/...`
预期：FAIL，编译错误 `undefined: NewPool` 等。

- [ ] **步骤 3：实现 `pkg/quota/quota.go`**

```go
// Package quota 提供通用配额池。不引入租户/存储概念。
package quota

import (
    "errors"
    "sync"
)

var ErrStorageFull = errors.New("storage quota exceeded")

// Pool 是配额池。可创建子 Scope（路径化叠加，参考 http 路由挂载语义）。
type Pool struct {
    mu         sync.RWMutex
    maxBytes   int64 // 0 = 不限制
    committed  int64
    reserved   int64
    children   []*Scope
}

func NewPool(maxBytes int64) *Pool { return &Pool{maxBytes: maxBytes} }

// Scope 返回一个关联父池的子作用域（路径化），子池操作自动向父链聚合。
func (p *Pool) Scope(path string, maxBytes int64) *Scope {
    s := &Scope{path: path, maxBytes: maxBytes, parent: p}
    p.mu.Lock(); p.children = append(p.children, s); p.mu.Unlock()
    return s
}

func (p *Pool) Usage() int64      { p.mu.RLock(); defer p.mu.RUnlock(); return p.committed }
func (p *Pool) Reserved() int64   { p.mu.RLock(); defer p.mu.RUnlock(); return p.reserved }
func (p *Pool) MaxBytes() int64   { return p.maxBytes }

// UsageByBucket 返回根池下所有子 Scope 的 committed 占用（按路径归集）。
func (p *Pool) UsageByBucket() map[string]int64 {
    m := map[string]int64{}
    p.collect(m)
    return m
}

type Scope struct {
    path      string
    maxBytes  int64
    committed int64
    reserved  int64
    parent    *Pool // 非 nil 时聚合到父池
}

func (s *Scope) TryReserve(estimate int64) (*Reservation, error) {
    if estimate < 0 { estimate = 0 }
    if err := s.reserve(estimate); err != nil { return nil, err }
    return &Reservation{scope: s, amount: estimate}, nil
}

func (s *Scope) reserve(n int64) error {
    // 自身上限检查
    if s.maxBytes > 0 {
        s.lock(); cur := s.committed + s.reserved + n; s.unlock()
        if cur > s.maxBytes { return ErrStorageFull }
    }
    // 父链兜底（全局）
    if s.parent != nil { if err := s.parent.reserveUp(n); err != nil { return err } }
    s.lock(); s.reserved += n; s.unlock()
    return nil
}

func (s *Scope) CommitReserved(amount int64) { /* 内部：reserved→committed 或 diff */ }
func (s *Scope) ReleaseReserved(amount int64) {}
func (s *Scope) ReleaseUsage(n int64)          { /* committed -= n + 父链回退 */ }
func (s *Scope) Adjust(prev, next int64)       { /* diff = next-prev，可正可负 */ }
func (s *Scope) Usage() int64
func (s *Scope) Reserved() int64
func (s *Scope) MaxBytes() int64
func (s *Scope) Available() int64              // max<=0 → MaxInt64；否则 max-(committed+reserved)
func (s *Scope) UsageByBucket() map[string]int64

// Reservation 是预留句柄。Commit(actual) 按实际对账；Release() 放弃预留。
type Reservation struct {
    scope  *Scope
    amount int64
    done   bool
}
func (r *Reservation) Commit(actual int64) { /* 内部按 actual 调整 diff */ }
func (r *Reservation) Release()            { /* 归还预留 */ }
```

实现要点：
- **锁**：`Scope` 与 `Pool` 各自的锁；子操作先查自身上限再向上聚合，聚合路径用 `reserveUp` 检查全局上限，避免调用方感知全局。
- **`Commit(actual)`**：`reserved` 先减 `amount`，再按 `actual` 计入 `committed`，diff 部分同步到父链（`actual > amount` 时补占，`actual < amount` 时归还）。
- **`Adjust(prev, next)`**：直接对 `committed` 加 `(next-prev)` 并同步父链，不经过 reserved。
- **并发**：所有计数在锁内；TryReserve 的双检查（自身 + 父链）在释放锁前闭合。

- [ ] **步骤 4：运行测试确认通过**

运行：`go test -count=1 -race ./pkg/quota/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/quota/
git commit -m "feat(quota): Pool/Scope/Reservation 通用配额池（路径化叠加+预留对账+Adjust）"
```

**出口标准：** 上述 5 个测试全绿；`ErrStorageFull` 哨兵错误；无租户/存储概念；`-race` 无告警。

---

### 任务 2：`pkg/store` + `pkg/store/file` — 通用记录存储

**目标：** 字节 KV 接口 + 泛型 `JSONStore[T]` + 插件注册表；默认文件实现（原子写 + 前缀遍历）。供 meta 记录与 mesh/hub 持久化复用。

**文件：**
- 创建：`pkg/store/store.go`、`pkg/store/json.go`、`pkg/store/file/file.go`
- 测试：`pkg/store/store_test.go`、`pkg/store/file/file_test.go`

- [ ] **步骤 1：编写失败测试**

```go
// pkg/store/store_test.go — 泛型 JSON 封装
func TestJSONStore_RoundTrip(t *testing.T) {
    dir := t.TempDir()
    st, err := file.New(StoreConfig{Root: dir})
    if err != nil { t.Fatal(err) }
    js := store.NewJSON[CloudTask](st)   // 用本地测试结构体
    if err := js.Set("cloud/t1", &CloudTask{ID: "t1", Status: "completed"}); err != nil { t.Fatal(err) }
    got, err := js.Get("cloud/t1")
    if err != nil || got.Status != "completed" { t.Fatalf("Get=%+v err=%v", got, err) }
    if err := js.Delete("cloud/t1"); err != nil { t.Fatal(err) }
    if _, err := js.Get("cloud/t1"); !errors.Is(err, os.ErrNotExist) { t.Fatalf("删除后应不存在, err=%v", err) }
}

func TestJSONStore_ListPrefix(t *testing.T) {
    st, _ := file.New(StoreConfig{Root: t.TempDir()})
    js := store.NewJSON[Task](st)
    js.Set("cloud/a", &Task{ID: "a"})
    js.Set("cloud/b", &Task{ID: "b"})
    js.Set("sync/c", &Task{ID: "c"})
    got, _ := js.List("cloud/")
    if len(got) != 2 { t.Fatalf("List(cloud/)=%d want 2", len(got)) }
}

// pkg/store/file/file_test.go — 原子写
func TestFileStore_AtomicWrite(t *testing.T) {
    dir := t.TempDir()
    st, _ := file.New(StoreConfig{Root: dir})
    if err := st.Set("k/v", []byte("value")); err != nil { t.Fatal(err) }
    data, _ := st.Get("k/v")
    if string(data) != "value" { t.Fatalf("got %q", data) }
    // 无 tmp 残留
    if _, err := os.Stat(filepath.Join(dir, "k", "v.tmp")); !os.IsNotExist(err) {
        t.Fatalf("不应残留 tmp 文件")
    }
}

func TestFileStore_CrashResidueCleaned(t *testing.T) {
    dir := t.TempDir()
    os.MkdirAll(filepath.Join(dir, "k"), 0o755)
    os.WriteFile(filepath.Join(dir, "k", "v.tmp"), []byte("junk"), 0o644) // 模拟崩溃残留
    st, _ := file.New(StoreConfig{Root: dir})
    data, err := st.Get("k/v")
    if !os.IsNotExist(err) { t.Fatalf("残留 tmp 应不影响读取, err=%v", err) }
}
```

- [ ] **步骤 2：运行测试确认失败**

运行：`go test -count=1 ./pkg/store/...`
预期：FAIL，编译错误 `undefined: file.New` 等。

- [ ] **步骤 3：实现三个文件**

`pkg/store/store.go`：
```go
// Package store 提供通用记录存储：字节级 KV 接口 + 插件注册表。
package store

type Store interface {
    Get(key string) ([]byte, error)
    Set(key string, value []byte) error // 原子写（实现负责 tmp+rename+锁）
    Delete(key string) error
    List(prefix string) ([][]byte, error)
    Close() error
}

type StoreConfig struct { Root string }

type Factory func(cfg StoreConfig) (Store, error)

var registry = map[string]Factory{}
func Register(name string, f Factory)
func Open(name string, cfg StoreConfig) (Store, error)
```

`pkg/store/json.go`：
```go
package store

import "encoding/json"

// JSONStore 提供类型安全的原子 JSON 记录读写。
type JSONStore[T any] struct{ s Store }
func NewJSON[T any](s Store) *JSONStore[T]
func (j *JSONStore[T]) Get(key string) (*T, error)      // 不存在 → nil, os.ErrNotExist
func (j *JSONStore[T]) Set(key string, v *T) error      // json.Marshal + s.Set
func (j *JSONStore[T]) Delete(key string) error
func (j *JSONStore[T]) List(prefix string) ([]*T, error)
```

`pkg/store/file/file.go`（默认实现）：
```go
// Package file 提供基于 JSON 文件的 Store 实现（原子写：tmp + rename + 锁）。
package file

// New 打开文件存储根。key 以 / 分隔；磁盘路径 = Root + 相对 key（禁止 key 逃逸，校验 ../、绝对路径）。
// Set 原子写：写 <key>.tmp 再 rename；持 saveMu 串行化（Windows rename 并发防护）。
// List(prefix)：前缀遍历对应目录，跳过 .tmp 残留。
func New(cfg store.StoreConfig) (store.Store, error)
```

实现要点：
- `file.New` 内 `store.Register("file", New)`（包 init 注册，插件模式示例）。
- **key 安全**：`file` 实现必须拒绝含 `..` 段、绝对路径、空段的 key（用与 `pkg/storage` 相同的段校验或独立实现，避免循环依赖——`pkg/store` 是叶子，不能依赖 `pkg/storage`；段校验逻辑在 `pkg/store/file` 内做轻量校验即可，严格段名校验在 `pkg/storage/name.go`，两者不冲突）。
- 原子写模式复用现有 `ChecksumStore.save` / `syncmgr` 写 session.json 的成熟模式（tmp + rename + 失败回退 WriteFile）。

- [ ] **步骤 4：运行测试确认通过**

运行：`go test -count=1 -race ./pkg/store/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/store/
git commit -m "feat(store): 通用记录存储（字节 KV + JSONStore[T] 泛型 + file 原子实现 + 插件注册表）"
```

**出口标准：** 测试全绿；`file` 实现原子写无 tmp 残留、前缀遍历正确；`store.Open("file", ...)` 可用。

---

### 任务 3：`pkg/storage` — 布局 + os.Root + 段名校验 + Tenant

**目标：** 存储布局领域包：`Root`（os.Root + LAYOUT_VERSION）、`Tenant`（布局/UserRel/FeatureRel）、`name.go` 段名校验单一权威。

**文件：**
- 创建：`pkg/storage/name.go`、`pkg/storage/root.go`、`pkg/storage/tenant.go`、`pkg/storage/path.go`
- 测试：`pkg/storage/name_test.go`、`pkg/storage/root_test.go`、`pkg/storage/tenant_test.go`

- [ ] **步骤 1：编写失败测试**

```go
// name_test.go — 段名校验（Windows 表驱动）
func TestValidSegmentName(t *testing.T) {
    cases := []struct{ in string; ok bool }{
        {"alice", true}, {"anonymous", true}, {"ak-abc123", true},
        {"", false}, {".", false}, {"..", false}, {"a/b", false}, {`a\b`, false},
        {"CON", false}, {"con", false}, {"CON.txt", false},   // 保留字含扩展名（基名判定）
        {"NUL", false}, {"PRN", false}, {"AUX", false},
        {"COM1", false}, {"lpt9", false}, {"COM10", true},    // COM10 合法
        {"foo.", false}, {"foo ", false},                     // 尾点/尾空格
        {"a:b", false}, {`a<b`, false}, {`a>b`, false}, {`a"b`, false},
        {"a|b", false}, {"a?b", false}, {"a*b", false},
        {".__cloud__", false},                                // 魔法前缀禁止
        {strings.Repeat("x", 256), false},                    // 超长
    }
    for _, c := range cases {
        if got := ValidSegmentName(c.in); got != c.ok {
            t.Fatalf("ValidSegmentName(%q)=%v want %v", c.in, got, c.ok)
        }
    }
}

// root_test.go — os.Root 防穿越
func TestRoot_RejectsTraversal(t *testing.T) {
    dir := t.TempDir()
    r, err := OpenRoot(dir)
    if err != nil { t.Fatal(err) }
    defer r.Close()
    for _, rel := range []string{"../etc/passwd", "a/../../etc", "/abs", `..\escape`} {
        if _, err := r.Open(rel); err == nil {
            t.Fatalf("穿越路径 %q 应被拒绝", rel)
        }
    }
    if err := r.MkdirAll("user/sub", 0o755); err != nil { t.Fatal(err) }
    f, err := r.OpenFile("user/sub/f.txt", os.O_CREATE|os.O_WRONLY, 0o644)
    if err != nil { t.Fatal(err) }
    f.Close()
    if _, err := r.Open("user/sub/f.txt"); err != nil { t.Fatalf("合法路径应可读: %v", err) }
}

// tenant_test.go — Tenant 布局与 rel 判定
func TestTenant_UserRel(t *testing.T) {
    tnt := newTestTenant(t, "alice")
    rel, ok := tnt.UserRel("dir/report.pdf")
    if !ok || rel != "user/dir/report.pdf" { t.Fatalf("UserRel=%q,%v", rel, ok) }
    if _, ok := tnt.UserRel("../etc"); ok { t.Fatalf("穿越应拒绝") }
    if _, ok := tnt.UserRel("__version__/x"); ok { t.Fatalf("非 user 桶前缀输入应拒绝") }
    if _, ok := tnt.UserRel("cloud/x"); ok { t.Fatalf("用户不得触碰功能桶") }
    if rel, ok := tnt.FeatureRel("cloud", "t1/f.bin"); !ok || rel != "cloud/t1/f.bin" {
        t.Fatalf("FeatureRel=%q,%v", rel, ok)
    }
    if _, ok := tnt.FeatureRel("bogus", "x"); ok { t.Fatalf("未知桶应拒绝") }
}
```

- [ ] **步骤 2：运行测试确认失败**

运行：`go test -count=1 ./pkg/storage/...`
预期：FAIL，`undefined: ValidSegmentName` 等。

- [ ] **步骤 3：实现四个文件**

`pkg/storage/name.go`（单一权威段名校验）：
```go
package storage

// ValidSegmentName 校验 name 是否可作为单个路径段（租户名、upload_id、文件名段共用）。
// 拒绝：空、. / ..、含 / 或 \、Windows 非法字符 <>:"|?*、以 .__ 开头（魔法前缀禁止）、
// Windows 保留设备名（基名判定：CON/NUL/PRN/AUX/COM1-9/LPT1-9，含 CON.txt 形式）、
// 尾点/尾空格（Windows 文件系统会剥除导致目录合并）、长度 > 255。
func ValidSegmentName(name string) bool
```

实现要点：基名判定取首个 `.` 前大写；`COM10`/`LPT10` 合法（精确 1-9）；尾点/尾空格 `strings.HasSuffix` 检查；魔法前缀 `strings.HasPrefix(name, ".__")` 拒绝（兼容遗留内部目录名）。

`pkg/storage/root.go`：
```go
// Root 封装 os.Root，附带布局版本校验。所有文件操作相对 root，防穿越由标准库保证。
const LayoutVersion = "2"

type Root struct {
    r *os.Root
    base string // 绝对路径（供 Chtimes 等 os.Root 未覆盖操作派生）
}

func OpenRoot(path string) (*Root, error) {
    // 1. os.OpenRoot(path)
    // 2. 读/写 LAYOUT_VERSION：不存在则写 LayoutVersion；存在且不匹配则报错（迁移钩子）
}
func (rt *Root) Open(rel string) (*os.File, error)
func (rt *Root) OpenFile(rel string, flag int, perm os.FileMode) (*os.File, error)
func (rt *Root) Stat(rel string) (os.FileInfo, error)
func (rt *Root) MkdirAll(rel string, perm os.FileMode) error
func (rt *Root) Remove(rel string) error
func (rt *Root) Rename(oldRel, newRel string) error
func (rt *Root) Chtimes(rel string, atime, mtime time.Time) error // os.Root 未覆盖：root 内已校验 rel 派生绝对路径 + 路径仍相对 base 校验
func (rt *Root) Close() error
func (rt *Root) Abs(rel string) (string, bool) // 派生绝对路径并确认在 base 内（供 os.SameFile 等）
```

`pkg/storage/tenant.go`：
```go
// NewTenant 构造租户值类型。owner 必须通过 ValidSegmentName（fail-closed），
// 非法 owner 返回错误（绝不回落全局根）。
func NewTenant(owner string, root *Root) (*Tenant, error)

// Tenant 持有自己的 *Root 与目录布局。不持有配额类型（避免 pkg/storage → pkg/quota 依赖）。
type Tenant struct {
    ID   string // 合法段名
    root *Root
}
```

实现要点：为避免 `pkg/storage → pkg/quota` 依赖，`Tenant` 不持有配额类型；配额由 `pkg/server` 用 `map[string]*quota.Scope` 按 tenant.ID 关联（server 装配层职责，见任务 4 的 `quotaFor`）。

```go
// tenant.go（续）
func (t *Tenant) Root() *Root
func (t *Tenant) UserRoot() string                  // "user"
func (t *Tenant) UserRel(remotePath string) (string, bool)      // 校验+归一 → "user/<path>"
func (t *Tenant) FeatureRel(bucket, sub string) (string, bool)  // bucket ∈ 白名单
func (t *Tenant) Buckets() []string                 // [user cloud archive chunk version meta]
func (t *Tenant) MetaKeys() map[string]string       // meta 桶 key 前缀（可选）
```

`pkg/storage/path.go`：
```go
// 协议路径归一：/ 作为唯一协议分隔符；磁盘路径经 root 内部 FromSlash。
func NormalizeRemote(remotePath string) (string, bool)  // TrimSpace、拒绝空、绝对路径、.. 段；返回 ToSlash
func JoinRel(segs ...string) string                     // 用 / 拼接（协议路径）
```

- [ ] **步骤 4：运行测试确认通过**

运行：`go test -count=1 -race ./pkg/storage/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/storage/
git commit -m "feat(storage): 布局领域包（os.Root 封装 + Tenant 布局 + 段名校验单一权威）"
```

**出口标准：** 测试全绿；`UserRel` 拒绝 `../`、绝对路径、`__version__`/`cloud` 等非 user 桶输入；`FeatureRel` 桶白名单；`ValidSegmentName` 表驱动覆盖 Windows 保留字/尾点/尾空格/大小写/分隔符/超长；os.Root 拒绝穿越与符号链接逃逸。

**P0 门禁：** 任务 1-3 全部完成 + 独立审查通过 + `go test -race ./pkg/quota/... ./pkg/store/... ./pkg/storage/...` 全绿。

---

## 阶段 P1：装配层

### 任务 4：配置变更 + 启动装配 + LAYOUT_VERSION

**目标：** 配置 `storage_root`/`owner_quotas`；启动时 OpenRoot + GlobalPool + 按 owner 创建租户 Scope；`LAYOUT_VERSION` 校验；`anonymous` 默认租户。

**文件：**
- 修改：`pkg/server/config.go`（新增字段）、`pkg/server/handlers.go`（装配）、`cmd/sproxy/root.go`（配置传递）
- 测试：`pkg/server/config_test.go`（新增）、`pkg/server/handlers_test.go`（装配辅助）

- [ ] **步骤 1：编写失败测试**

```go
// config_test.go
func TestConfig_StorageRoot(t *testing.T) {
    c := Default()
    c.UploadsDir = "./storage"
    if c.StorageRoot() != "./storage" { t.Fatalf("StorageRoot=%q", c.StorageRoot()) }
    // OwnerQuotaFor：未配置返回默认值
    if got := c.OwnerQuotaFor("alice"); got != 0 { t.Fatalf("OwnerQuotaFor(默认)=%d want 0", got) }
    c.OwnerQuotas = map[string]int64{"*": 5 << 30, "alice": 10 << 30}
    if got := c.OwnerQuotaFor("alice"); got != 10<<30 { t.Fatalf("OwnerQuotaFor(alice)=%d", got) }
    if got := c.OwnerQuotaFor("bob"); got != 5<<30 { t.Fatalf("OwnerQuotaFor(bob 默认*)=%d", got) }
}
```

- [ ] **步骤 2：运行测试确认失败**

运行：`go test -count=1 -run TestConfig_StorageRoot ./pkg/server/`
预期：FAIL，`undefined: StorageRoot`。

- [ ] **步骤 3：实现配置字段与装配**

`pkg/server/config.go`：
```go
// UploadsDir 保留字段（兼容读旧配置），新增：
StorageRoot   string           `yaml:"storage_root" mapstructure:"storage_root"` // 默认 "./storage"
OwnerQuotas   map[string]int64 `yaml:"owner_quotas" mapstructure:"owner_quotas"` // key 为 owner 名，"*" 为默认值

func (c *Config) StorageRoot() string // StorageRoot 非空优先，否则回退 UploadsDir
func (c *Config) OwnerQuotaFor(owner string) int64 // 显式 owner > "*" 默认 > 0
```

`pkg/server/handlers.go` 装配（RegisterRoutes 内）：
```go
// 1. 打开全局根
globalRoot, err := storage.OpenRoot(cfg.StorageRoot())
if err != nil { log.Error(...); /* 启动失败或布局版本不匹配 */ }

// 2. 全局配额池 + anonymous 租户
globalPool := quota.NewPool(cfg.MaxStorageBytes)
tenantRoots := map[string]*storage.Tenant{}
for _, owner := range allOwners {   // anonymous + 配置中出现的 owner（懒创建：首次请求时若不存在再建）
    t, err := storage.NewTenant(owner, globalRoot)
    if err != nil { /* 非法 owner：配置错误，启动日志告警，跳过 */ }
    tenantRoots[owner] = t
}
```

`cmd/sproxy/root.go`：把 `storage_root`/`owner_quotas` 接入 viper（前缀 `SPROXY_`），`LoadFromViper` 填充。

`pkg/server/handlers.go` 装配辅助方法（供 P2/P3 各 handler 任务复用，签名在此定稿）：
```go
// tenantOf 返回请求者 owner 的租户（owner 空 → anonymous 租户）。构造失败返回 nil（调用方按 400 处理）。
func (h *Handlers) tenantOf(r *http.Request) *storage.Tenant
func (h *Handlers) tenantFor(owner string) *storage.Tenant   // 懒创建：首次访问按 owner 建租户（ValidSegmentName fail-closed）
func (h *Handlers) checksumStoreFor(owner string) *ChecksumStore  // per-tenant checksum 缓存（meta/checksums.json）
func (h *Handlers) quotaFor(owner string) *quota.Scope            // per-tenant 配额 Scope（globalPool.Scope 懒创建）
```

- [ ] **步骤 4：运行测试确认通过**

运行：`go test -count=1 ./pkg/server/...`
预期：PASS（既有测试需适配：`Default()` 的 `UploadsDir` 兼容；`newTestCfgPtr` 等测试辅助同步 `StorageRoot`）

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/config.go pkg/server/handlers.go cmd/sproxy/root.go pkg/server/config_test.go
git commit -m "feat(server): 配置 storage_root/owner_quotas + 启动装配 GlobalRoot/GlobalPool/租户"
```

**出口标准：** 配置解析 + `OwnerQuotaFor` 默认值逻辑测试通过；启动成功（OpenRoot + LAYOUT_VERSION 写入）；`healthz` 正常；`anonymous` 租户根创建。

---

## 阶段 P2：用户文件链路迁移（handler 逐个切到 Tenant API）

**通用迁移模式**（每个 handler 任务遵循，禁止占位符，逐 handler 给出具体变化）：
1. 保留 handler 对外语义（HTTP 路由、请求参数、响应 JSON、错误消息）。
2. 替换路径解析：`ValidateFilePath(x) + isInternalDirPathPrefix(x) + h.safePathFor(r, x)` → `h.tenantOf(r).UserRel(x)`。
3. 替换磁盘操作：`os.Open/Stat/MkdirAll/Remove/Rename/Chtimes(absPath)` → `tenant.Root().Open/Stat/MkdirAll/Remove/Rename/Chtimes(rel)`。
4. 替换 checksum key：`h.checksumKeyFor(r, remotePath)` → 租户 `meta/checksums.json` 的 `JSONStore[string]`，key = `UserRel` 返回的 rel（如 `user/dir/f.txt`）。
5. `checksumStoreKey(owner, path)` owner 参数删除；`ChecksumStore` 从全局实例改为**每租户一个**（server 持有 `map[owner]*ChecksumStore`，key 相对租户根）。
6. 每迁移一个 handler：跑对应既有测试（`go test -count=1 -run <TestName> ./pkg/server/`）+ 新增新布局断言（文件落在 `user/` 下、checksum key 无 owner 前缀）。

### 任务 5：upload 链路

**文件：** 修改 `pkg/server/upload_handler.go`、`pkg/server/checksum_store.go`（改 per-tenant 装配）；测试 `pkg/server/chunked_owner_test.go`、`pkg/server/upload_handler_test.go`

- [ ] **步骤 1：编写失败测试**——上传后断言磁盘落 `user/` 下、checksum key 为 `user/<rel>` 且无 owner 前缀

```go
func TestUpload_NewLayoutDiskPath(t *testing.T) {
    env := newOwnerEnv(t)   // 适配新装配：StorageRoot + tenantRoots
    // 以 alice 上传 dir/f.txt
    ...  // 复用现有上传测试辅助
    // 断言文件在 <root>/alice/user/dir/f.txt，而非 <root>/alice/dir/f.txt
    if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); err != nil {
        t.Fatalf("文件应落在 alice/user/dir/f.txt: %v", err)
    }
    // 断言 checksum key 无 owner 前缀
    if _, ok := env.checksumStoreFor("alice").Get("user/dir/f.txt"); !ok {
        t.Fatalf("checksum key 应为 user/dir/f.txt")
    }
}
```

- [ ] **步骤 2：运行测试确认失败**
运行：`go test -count=1 -run TestUpload_NewLayoutDiskPath ./pkg/server/` → FAIL

- [ ] **步骤 3：迁移 upload 链路**

`upload_handler.go`：
- `resolveFilePath` 改为：`rel, ok := h.tenantOf(r).UserRel(filename)`；`os.MkdirAll(filepath.Dir(...))` → `root.MkdirAll(rel 的父目录)`；`writeFileAtomically` 改为接收 `*Root` + `rel`（`CreateTemp` 经 root，或 root 内派生绝对路径 + 原子 rename 仍用绝对路径——见 root.Abs 辅助）。
- `setUploadResponseHeaders` 的 `checksumStore.Set(key, ...)`：key = `rel`，store = `h.checksumStoreFor(owner)`。
- `handleDuplicateFile` 的 `os.Stat(filePath)` → `root.Stat(rel)`。
- 并发防护 `ownerScopedUploadKey(owner, remotePath)` 删除 owner 参数：key = rel（租户隔离由 per-tenant `uploadingFiles` 或 rel 已含租户桶保证——建议 `uploadingFiles` 改为 per-tenant map 或 key=tenantID+"\x00"+rel）。

- [ ] **步骤 4：运行测试确认通过**
运行：`go test -count=1 -run 'TestUpload|TestChunkedUploadOwner' ./pkg/server/` → PASS
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): upload 链路迁移到 Tenant API（user/ 根 + per-tenant checksum）"`

### 任务 6：download / stat / chunk 下载链路

**文件：** `pkg/server/download_handler.go`、`pkg/server/chunked_download.go`；测试 `pkg/server/safepath_test.go`、`cloud_owner_test.go`

- [ ] **步骤 1：写失败测试**——`TestDownload_RejectsInternalDirPrefix` 更新：内部目录判定改为"非 user 桶前缀拒绝"（`cloud/...`、`archive/...` 直接拒绝）

```go
// 更新既有测试：普通下载请求 cloud/xx 必须 400；user/xx 正常
func TestDownload_NonUserBucketRejected(t *testing.T) {
    env := newOwnerEnv(t)
    for _, name := range []string{"cloud/t1/f.bin", "archive/x.tar.gz", "chunk/s/00000.chunk", "meta/cloud/t1.json"} {
        code := env.doGet(t, "alice", "/download?filename="+url.QueryEscape(name))
        if code != http.StatusBadRequest { t.Fatalf("%q 应 400, got %d", name, code) }
    }
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`resolveDownloadPath` 空 kind 分支：`ValidateFilePath(name)` + `tenant.UserRel(name)`；`download`/`stat` 的 checksum 读改用 per-tenant store + `rel` key；`chunked_download.go` 同；`downloadKindCloudArchive`/`downloadKindCloudTask` 分支在 P3（任务 13/14）迁移，此处先保持旧路径解析到新桶（`FeatureRel("archive", name)`、`FeatureRel("cloud", taskID+"/"+file)`）。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestDownload|TestStat|TestSafePath|TestCloudOwner' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): download/stat/chunk 链路迁移到 Tenant API"`

### 任务 7：delete / rename / batch

**文件：** `pkg/server/delete_handler.go`、`pkg/server/rename_handler.go`；测试既有 delete/rename/batch 测试

- [ ] **步骤 1：写失败测试**——删除后 per-tenant checksum key 被清理（无 owner 前缀）

```go
func TestDelete_NewLayoutChecksumCleaned(t *testing.T) {
    env := newOwnerEnv(t)
    // 上传 user/dir/f.txt → 删除 → 断言 per-tenant store 中 "user/dir/f.txt" 已删除
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`resolveAndValidateFile`/`resolveAndValidateFileForOwner` → `tenantFor(owner).UserRel`；`resolveRenamePaths` → `UserRel(from)/UserRel(to)` + `root.Rename`；`isInternalDirPathPrefix` 守卫删除（UserRel 已保证 user 桶）；checksum `Rename/Delete` 用 per-tenant store + rel key；`rmdir` 的 `DeletePrefix` → per-tenant store 删除 `rel/` 前缀。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestDelete|TestRename|TestBatch' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): delete/rename/batch 链路迁移到 Tenant API"`

### 任务 8：dirs（mkdir / rmdir）

**文件：** `pkg/server/dirs.go`；测试既有 mkdir/rmdir 测试 + 新增守卫断言

- [ ] **步骤 1：写失败测试**——`mkdir dirname=cloud` 应成功（用户目录在 `user/` 桶内，`user/cloud` 与功能桶 `cloud` 不同路径不冲突），且落盘 `user/cloud`、列表在 `user/` 桶内可见、功能桶顶层不可枚举

```go
func TestMkdir_UserBucketUnderUser(t *testing.T) {
    env := newOwnerEnv(t)
    // 用户在 user/ 桶内创建 "cloud" 目录 → 200，落盘 <root>/alice/user/cloud
    if code := env.doPost(t, "alice", "/mkdir?dirname=cloud"); code != http.StatusOK {
        t.Fatalf("mkdir cloud 应 200, got %d", code)
    }
    if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "cloud")); err != nil {
        t.Fatalf("应落盘 alice/user/cloud: %v", err)
    }
    // 顶层列表不应暴露功能桶（cloud/archive/chunk/version/meta 顶层不可枚举）
    body := env.doGet(t, "alice", "/api/files")
    for _, bucket := range []string{"cloud", "archive", "chunk", "version", "meta"} {
        if strings.Contains(body, `"`+bucket+`"`) {
            t.Fatalf("顶层列表不应出现功能桶 %q: %s", bucket, body)
        }
    }
}
```

- [ ] **步骤 2：跑失败** → FAIL（当前 `mkdir dirname=cloud` 落盘 `<root>/alice/cloud`，与功能桶同层）
- [ ] **步骤 3：迁移**：`UserRel` 把用户路径统一加 `user/` 前缀（`UserRel("cloud")` → `user/cloud`）；`safePathFor` → `UserRel` + `root.MkdirAll`/`root.RemoveAll`；`resolveListDir` 默认根 = `user/` 桶，功能桶不在其下天然不可枚举。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestMkdir|TestRmdir' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): mkdir/rmdir 迁移到 Tenant API（user 桶内创建）"`

### 任务 9：list / search

**文件：** `pkg/server/list_handler.go`；测试既有 list/search + 新增隔离断言

- [ ] **步骤 1：写失败测试**——`subdir=cloud` 顶层必须返回空（功能桶不可枚举，修复 R1）

```go
func TestList_FeatureBucketsHidden(t *testing.T) {
    env := newOwnerEnv(t)
    // 顶层列表不应出现 cloud/archive/chunk/version/meta
    // subdir=user 正常；subdir=cloud 返回空（不存在该路径）
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`resolveListDir` 默认根 = `tenant.UserRoot()` 的绝对路径（或 root 内 `user`）；`subdir` 经 `UserRel`（相对 user 根拼接）；`isInternalDir`/`isInternalFirstName` 删除——列表条目天然在 user 桶内，功能桶不在根下；search 的 `isInternalDirPathPrefix` 删除，根 = `user` 桶；checksum 读用 per-tenant store + `user/` rel。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestList|TestSearch|TestCloudOwner' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): list/search 迁移到 Tenant API（user 桶根 + 功能桶不可枚举）"`

### 任务 10：share

**文件：** `pkg/server/share.go`；测试既有 share 测试

- [ ] **步骤 1：写失败测试**——创建分享后存储 rel，访问按 rel 解析

```go
func TestShare_NewLayoutResolvesUserRel(t *testing.T) {
    env := newOwnerEnv(t)
    // 创建分享 user/dir/f.txt → token
    // 经 /s/{token} 访问应返回内容
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`createShareHandler` 的 `fullPath` → `tenant.UserRel(req.Filename)` 的 rel + root；`ShareStore` 存储 rel + tenantID（不再存绝对路径）；`accessShareHandler` 用 tenantID 找租户 root + 校验 rel 仍属 user 桶 + `root.Open(rel)`（TOCTOU 收敛）。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run TestShare ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): share 迁移到 Tenant API（存 rel + tenantID，访问经 root）"`

### 任务 11：version

**文件：** `pkg/server/version.go`；测试既有 version 测试

- [ ] **步骤 1：写失败测试**——版本文件落 `version/<userRel>/<id>`、checksum key 为 `version/<userRel>/<id>`

```go
func TestVersion_NewLayout(t *testing.T) {
    env := newOwnerEnv(t)
    // 启用 versioning，上传 user/dir/f.txt，再覆盖 → 断言
    // 版本文件在 <root>/alice/version/dir/f.txt/<versionID>
    // checksum key = "version/dir/f.txt/<id>"（per-tenant store）
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`saveVersion` 的 `versionsDirName`（`.__versions__`）→ `FeatureRel("version", userRel)`；`listVersionsHandler`/`restore`/`delete` 同；`__version__` checksum key 改 `version/<rel>/<id>`（消除 R4 碰撞）；`saveVersionBeforeOverwrite` 用 `UserRel`。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestVersion|TestUpload' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): version 迁移到 Tenant API（version/ 桶 + checksum key 消除碰撞）"`

**P2 门禁：** 任务 5-11 全部完成 + 独立审查 + `go test -count=1 ./pkg/server/` 全绿 + 无 `safePathFor`/`checksumKeyFor` 残留调用（grep）。

---

## 阶段 P3：功能模块迁移

### 任务 12：chunked_upload（UploadStore per-tenant chunk 根 + 去 owner 前缀）

**文件：** `pkg/server/chunked_upload.go`、`pkg/server/upload_store.go`；测试 `pkg/server/chunked_owner_test.go`

- [ ] **步骤 1：写失败测试**——upload_id 不再带 owner 前缀；会话落 `alice/chunk/<id>/`

```go
func TestChunked_NewLayoutNoOwnerPrefix(t *testing.T) {
    env := newOwnerEnv(t)
    code, resp := env.initAs(t, "alice", "bareid123", "user/dir/f.bin", ...)
    // upload_id 返回 bareid123（无 alice/ 前缀）
    // 会话目录在 <root>/alice/chunk/bareid123/
    // chunk/complete/status 用裸 id 成功
    // 跨租户：alice 用 bob 的裸 id 会话 → 404（租户根隔离）
}
```

- [ ] **步骤 2：跑失败** → FAIL（当前返回 alice/ 前缀）
- [ ] **步骤 3：迁移**：
  - `UploadStore` 改为接收 `*storage.Root`（或 per-tenant baseDir）：`baseDir` = 租户根内 `chunk` 桶。`NewUploadStore(root, "chunk", ...)`；`ChunkFilePath(uploadID, i)` = `filepath.Join(baseDir, uploadID, chunkIndexFilename(i))`（baseDir 已是 root 内绝对路径）。
  - `ownerScopedSessionKey`/`validateSessionOwner`/`validUploadID` 删除：upload_id 直接用裸 id（仍过 `ValidSegmentName` 防路径穿越）；`uploadSessions` 过滤改为"只列本租户会话"（per-tenant UploadStore 实例天然隔离）。
  - `GetSessionByFilenameOwner`/`sessionOwnerMatches` 删除（per-tenant 实例无需 owner 匹配）。
  - `recoverOwnerSessions` 删除（chunk 根下直接是会话目录，恢复走原 `recoverSessions` 单层）。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestChunked|TestUpload' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): chunked_upload 迁移到 Tenant chunk 桶（去 owner 前缀 + per-tenant UploadStore）"`

### 任务 13：cloud_download（落 tenant/cloud + meta/cloud）

**文件：** `pkg/server/cloud_download.go`、`pkg/server/cloud_download_handler.go`；测试 `cloud_download_test.go`、`cloud_owner_test.go`

- [ ] **步骤 1：写失败测试**——云任务文件落 `alice/cloud/<taskID>/<file>`、状态落 `alice/meta/cloud/<taskID>.json`

```go
func TestCloud_NewLayout(t *testing.T) {
    env := newOwnerEnv(t)
    // 创建任务（owner=alice）→ 落盘 <root>/alice/cloud/<taskID>/<file>
    // 状态文件 <root>/alice/meta/cloud/<taskID>.json
    // kind=cloud_task 下载 filename=<taskID>/<file> 仍可用
    // bob 不能下载 alice 的任务（SnapshotTask owner 过滤保持）
}
```

- [ ] **步骤 2：跑失败** → FAIL（当前落全局 `.__cloud__/`）
- [ ] **步骤 3：迁移**：
  - `CloudDownloadManager` 构造改为接收 per-tenant 根解析函数或 tenant 映射：`m.cloudDir` 从 `filepath.Join(uploadsDir, cloudDirName)` 改为 `tenantRoot/<tenant>/cloud`（按任务 owner 解析）；`persistDir` → `tenantRoot/<tenant>/meta/cloud`。
  - `CreateTask` 时 owner 已注入（`req.Owner = ActorFrom`），写路径按 `t.Owner` 解析租户根；空 owner 任务在 anonymous 租户。
  - checksum 写端 `checksumStoreKey(t.Owner, ...)` → per-tenant store + `cloud/<taskID>/<file>` key。
  - 恢复逻辑 `recoverTasks`：遍历所有租户的 `meta/cloud/`（server 装配时把租户列表传给 manager）。
  - `resolveDownloadPath` kind=cloud_task 分支改 `FeatureRel("cloud", taskID+"/"+file)` + `root.Open`；校验任务属当前 owner 逻辑保持。
  - group 状态 `meta/cloud/groups/` 同。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestCloud|TestCloudOwner' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): cloud_download 迁移到 Tenant cloud/meta 桶（按 owner 落盘 + per-tenant checksum）"`

### 任务 14：cloud_archive（落 tenant/archive）

**文件：** `pkg/server/cloud_archive_handler.go`；测试 `cloud_archive_handler_test.go`

- [ ] **步骤 1：写失败测试**——归档落 `alice/archive/<name>.tar.gz`；已存在 409 检查落 archive 桶

```go
func TestCloudArchive_NewLayout(t *testing.T) {
    env := newOwnerEnv(t)
    // 创建归档 → <root>/alice/archive/xxx.tar.gz
    // 同名预置在 alice/archive/ → 409
    // kind=cloud_archive 下载 filename=xxx.tar.gz → 200
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：`cloudArchiveTask`/`cloudArchiveBatch`/`cloudArchiveGroup` 的 `archiveDir` 从 `filepath.Join(uploadsDir, cloudArchiveDirName, cloudArchiveOwnerDir(owner))` 改为 `tenantFor(owner).Root()` 内 `archive` 桶 + `root.OpenFile(O_EXCL)`；`cloudArchivePathFor` 改 `FeatureRel("archive", name)`；配额对账改 P4 的 `Reservation.Commit(actual)`（可先保留现有 pre/release 模式，P4 统一换）。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestCloudArchive|TestCloudOwner' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "refactor(server): cloud_archive 迁移到 Tenant archive 桶"`

### 任务 15：syncmgr（user 根 + meta/sync）

**文件：** `pkg/server/syncmgr/manager.go`、`pkg/syncexec/executor.go`、`pkg/server/sync_handler.go`；测试 `syncmgr/manager_test.go`、`pkg/syncexec/executor_test.go`

- [ ] **步骤 1：写失败测试**——pull 落盘在 `<tenant>/user/`、状态在 `<tenant>/meta/sync/`

```go
func TestSync_NewLayout(t *testing.T) {
    // 创建 pull 任务（owner=alice）→ 落盘 <root>/alice/user/<dst>
    // 状态文件 <root>/alice/meta/sync/<taskID>.json
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：迁移**：
  - `syncmgr` 从 `pkg/storage` 取租户根：`OwnerFileRoot`/`validOwnerName` 删除 → `storage.NewTenant(owner, globalRoot).Root()`（`pkg/syncexec` 的 `e.UploadsDir` 改为注入 per-tenant `*storage.Root` 或绝对 user 根）。
  - `persistDir` 从 `filepath.Join(uploadsDir, syncDirName)` 改为 per-tenant `meta/sync`；`recoverTasks` 遍历租户。
  - `validateSyncPath` 的 `.__` 段拒绝逻辑删除（用户路径在 user 桶内天然不涉功能桶；仍拒绝 `..`/绝对路径）。
  - `syncQuotaAdapter` 删除 → 直接用 `*quota.Scope`（P4 接线）。
  - 依赖 `pkg/storage`（`storage.NewTenant`）与 `pkg/quota`（Scope），消除 `validOwnerName` 漂移（W5）。
- [ ] **步骤 4：跑通过**：`cd pkg/server/syncmgr && go test -count=1 ./...`；`cd pkg/syncexec && go test -count=1 ./...`
- [ ] **步骤 5：Commit** `git commit -m "refactor(syncmgr): 迁移到 Tenant user/meta 桶（依赖 storage/quota，消除 validOwnerName 漂移）"`

**P3 门禁：** 任务 12-15 完成 + 独立审查 + 对应测试全绿 + 无 `.__cloud__`/`.__chunked__`/`.__sync__`/`.__downloads__`/`.__cloud_archives__` 常量残留引用（grep）。

---

## 阶段 P4：配额接线

### 任务 16：写路径配额接入（TryReserve/Commit/ReleaseUsage/Adjust）

**文件：** `pkg/server/upload_handler.go`、`chunked_upload.go`、`cloud_download.go`、`cloud_archive_handler.go`；测试新增配额对账断言

- [ ] **步骤 1：写失败测试**——上传预留→Commit、覆盖 Adjust、删除 ReleaseUsage、超租户上限拒绝

```go
func TestQuota_UploadCommitAndDelete(t *testing.T) {
    env := newOwnerEnv(t)
    env.setOwnerQuota("alice", 100)   // 租户上限 100
    // 上传 60 字节 → tenant scope Usage()==60
    // 覆盖为 40 → Adjust diff -20 → Usage()==40
    // 删除 → ReleaseUsage → Usage()==0
    // 另一租户 bob 配额独立，alice 打满不影响 bob
}

func TestQuota_TenantLimitRejected(t *testing.T) {
    env := newOwnerEnv(t)
    env.setOwnerQuota("alice", 10)
    // 上传 20 字节 → 507 InsufficientStorage
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：实现**：把 `h.storageMgr.TryReserve(...)` 调用点替换为租户 `Scope` 句柄：
  - upload：`TryReserve(est)` → `writeFileAtomically` 后 `res.Commit(actual)`（文件尺寸）；校验失败 `res.Release()`。
  - chunked_upload：init 预留 `TryReserve(est)`，complete 后 `Commit(actual)`；过期/删除会话 `ReleaseUsage` 或 `Release`。
  - cloud_download：下载前 `TryReserve(placeholder)`，完成后 `Commit(actual)`；failTask `ReleaseUsage`。
  - cloud_archive：`TryReserve(pre)` → `createTarGz` 后 `Commit(actual)`；已存在/失败 `Release()`（消除三段式对账）。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestQuota|TestUpload|TestCloud|TestChunked' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "feat(server): 写路径配额接入 Scope（TryReserve/Commit/ReleaseUsage/Adjust）"`

### 任务 17：stats 按租户 + StorageManager 拆解

**文件：** `pkg/server/stats.go`、`pkg/server/storage_manager.go`；测试 `stats` 相关

- [ ] **步骤 1：写失败测试**——认证用户 stats 只含本租户占用；分类按桶归集

```go
func TestStats_OwnerScopedUsage(t *testing.T) {
    env := newOwnerEnv(t)
    // alice 上传 60 + 云任务 40（cloud 桶）
    // alice stats: storage_user_files=60, storage_cloud=40, usage=100
    // bob stats: 全部为 0（隔离）
}
```

- [ ] **步骤 2：跑失败** → FAIL
- [ ] **步骤 3：实现**：
  - `StorageManager` 拆除：`ScanAndRecalculate` 的全局扫描改为按租户桶归集（每个租户 `user/cloud/archive/chunk/version` 桶各自计数，喂给对应 Scope 的 `Adjust` 校准）；保留顶层 `GlobalPool.Usage()` 聚合视图。
  - `statsHandler`：认证用户 → 本租户 Scope 的 `Usage()/UsageByBucket()`；空 owner（admin）→ GlobalPool。
  - `hasInternalDirAtAnyDepth`/分类逻辑删除 → 桶前缀判定（`user/`、`cloud/`、`archive/`、`chunk/`、`version/`）。
  - `walkUploadStatsByCategory` 改为按桶前缀分类。
- [ ] **步骤 4：跑通过**：`go test -count=1 -run 'TestStats|TestStorage' ./pkg/server/`
- [ ] **步骤 5：Commit** `git commit -m "feat(server): stats 按租户归集（Scope.UsageByBucket）+ StorageManager 拆 GlobalPool/Scope"`

**P4 门禁：** 任务 16-17 完成 + 独立审查 + 配额/统计测试全绿 + `storageMgr.TryReserve` 无残留调用点（grep）。

---

## 阶段 P5：删除旧实现 + 收尾

### 任务 18：删除旧实现

**文件：** 删除 `pkg/server/owner_path.go`；修改 `pkg/server/validate.go`（删 `isInternalFirstName`/`isInternalDirPathPrefix`/`joinSafePath`/`IsPathWithin`，保留 `ValidateFilePath` 或改由 `storage.NormalizeRemote` 取代）；删除 `.__xx__` 常量（`cloudDirName`/`downloadsDirName`/`versionsDirName`/`chunkedDirName`/`cloudArchiveDirName`/`syncDirName`）；清理 `checksumStoreKey`/`ownerScoped*`/`validOwnerDirName` 全部残留。

- [ ] **步骤 1：写失败测试（删除收尾）**

```go
// 用 grep 断言无旧符号残留（作为脚本检查，非 Go 测试）：
//   git grep -n "isInternalDirPathPrefix\|joinSafePath\|checksumStoreKey\|ownerScoped\|validOwnerDirName\|\.__cloud__\|\.__chunked__\|\.__versions__\|\.__downloads__\|\.__cloud_archives__\|\.__sync__" -- pkg/ cmd/
// 预期：0 命中（测试文件中的历史注释除外）
```

- [ ] **步骤 2：全量测试**

运行：`go test -count=1 ./pkg/server/... ./pkg/syncexec/... ./pkg/sync/... ./pkg/client/... ./pkg/...`；`cd pkg/server/syncmgr && go test -count=1 ./...`
预期：全绿

- [ ] **步骤 3：实现**：删除文件与符号；`validate.go` 清理后保留纯校验原语（若 `ValidateFilePath` 仍被 client/sync 引用则保留并注释指向 `storage` 包）；`handlers.go` 中 `ChecksumStore`/`UploadStore`/`CloudDownloadManager`/`SyncManager` 装配全部指向 per-tenant 实例。
- [ ] **步骤 4：E2E + lint + 构建**

运行：`make build-all`、`make lint`（多 module：`golangci-lint run ./pkg/... ./cmd/...`）、`go test -count=1 ./test/...`
预期：构建通过、lint 0 issues、E2E 通过（含多租户隔离 E2E：alice/bob/anonymous 互不可见）

- [ ] **步骤 5：Commit** `git commit -m "refactor(server): 删除旧存储布局实现（owner_path/魔法目录/joinSafePath/checksumStoreKey owner 前缀）"`

**P5 门禁：** 全量测试 + E2E + lint 全绿；grep 无旧符号残留；设计文档第 9 节边界清单逐项核对通过（独立审查 agent 对照）。

---

## 收尾清单（全部完成后核对）

- [ ] 设计文档第 9 节边界场景清单逐项打勾（路径/名称/租户/配额/恢复/并发/功能联动）
- [ ] `make build-all` + `make lint` + `go test -race ./pkg/... ./cmd/...` 全绿
- [ ] E2E 多租户隔离验证（alice/bob/anonymous）
- [ ] `pkg/client`/`cmd/sclient` 未改动、API 兼容（remotePath 语义不变）
- [ ] 更新 `docs/architecture.md`（存储布局章节）、`docs/config.md`（`storage_root`/`owner_quotas`）、根/子 `CLAUDE.md`（`uploads_dir` → `storage_root`）
- [ ] 每个 PR 合入 master 前派独立审查 agent（`independent-review-before-merge`）
