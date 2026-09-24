# 设计：WebDAV LOCK 持久化（roadmap 11.5-③，延后 P3 蓝图）

- 状态：延后（P3）| 日期：2026-09-24 | 功能编号：11.5-③
- 现状：pkg/gateway/webdav/webdav.go:47 `webdav.NewMemLS()` —— x/net/webdav 的内存锁
  系统，锁状态存进程内存，重启/多实例即丢失；单实例部署下锁丢失被协议容忍
  （客户端超时后重新 LOCK），但多实例（同一 sync.FS 后端负载均衡）会出现两实例
  互不知晓对方锁的假互斥。
- 确认：单实例重启丢锁 = 已接受的协议容忍（RFC 4918 允许），本期不做；本蓝图评估
  持久化锁系统的完整方案，作为 P3 候选实施依据。

## 1. 组件与接口

- 新包 pkg/gateway/webdav/lockstore（纯标准库）：
  - type Lock struct{ Token, Owner, Root, Depth, Expiry string; ExpiresAt, CreatedAt
    time.Time }
  - type Store interface {
      Create(l Lock) error                       // 冲突（已锁/过期未清）→ ErrConflict
      Refresh(token string, newExpiry time.Time) error
      Unlock(token string) error
      List() ([]Lock, error)
    }
  - type MemStore struct{}                       // 现 NewMemLS 语义等价物（测试/单实例默认）
  - type FileStore struct{ dir string }          // 持久化实现：JSON 行文件
- webdav.go 集成：NewHandler(fs, store Store)（新签名，向后兼容：nil → 内部 MemStore），
  以 store 桥接 x/net/webdav.LockSystem（实现 Create/Refresh/Unlock 3 方法；
  x/net/webdav 的 LockSystem 接口本就只含这 3 个，天然可适配）。
- 配置：webdav 网关配置段加 lock.persist（路径，空=内存）/ lock.ttl（默认 24h）/
  lock.gc_interval（默认 1h）。

## 2. 数据流

1. LOCK 请求 → LockSystem.Create → FileStore 写 JSON 行（原子：写 tmp + rename）+ 更新
   内存索引（Read 时惰性加载目录一次，Watch 简单化：写时全量重读或增量刷索引）。
2. UNLOCK/REFRESH → 按 token 定位 → 更新/删除对应行 + 原子写。
3. 后台 GC：定时扫描过期锁（ExpiresAt 已过）→ 删除并日志；进程启动时加载目录内全部
   锁并立即清一次过期（崩溃残留自愈）。
4. 读路径（PROPFIND/GET/PUT 的锁检查）：x/net/webdav 内部走 LockSystem 查询，桥接层
   从内存索引取（写路径保持内存索引与文件一致，读不落盘）。

## 3. 错误处理

- 锁冲突：Create 返回 ErrConflict → 423 Locked（x/net/webdav 已有映射）。
- 文件写失败（磁盘满/权限）：返回错误 → 该 LOCK 请求失败，锁不生效（fail-closed，
  禁止「内存成功、落盘失败」的静默不一致）。
- 目录不存在：启动时 MkdirAll。
- 单文件损坏：跳过该行 + 日志 WARN（不整体拒绝，防单行损坏拖垮全部锁）。
- TTL 过期：GC 与查询时都判定过期即视同不存在（惰性清理）。

## 4. 测试与变异点

- 本项是延后蓝图：产出「文档断言测试」internal/archcheck/docs_lock_persist_test.go，
  断言设计文档存在且包含「ErrConflict 无静默不一致」关键词（防方案悄悄漂移）。
- 蓝图内预演单测（实施时 TDD）：
  - FileStore 往返（Create→List→Refresh→Unlock）+ 重启模拟（新实例重读）；
  - 冲突与过期判定（变异：ExpiresAt 比较用 >= 而非 > → 红）；
  - GC 清理 + 损坏行跳过（变异：去掉损坏容错 → 红）。

## 5. 片划分（P3 实施时）

- 片1：lockstore 包（MemStore+FileStore）+ 单测 + 变异 → 独立 PR。
- 片2：webdav.go 换签名 + LockSystem 桥接 + 配置接线 + 集成测试（两实例互斥实测）。
- 片3：GC 后台任务 + 崩溃恢复测试 + docs。

## 6. 风险与零回归

- NewHandler 签名变更：唯一调用方在 pkg/gateway/webdav（cmd/sproxy 装配层），
  nil 默认保持现有内存行为 → 对外 http.Handler 行为零变化。
- 锁持久化不改变文件读写路径（readFile/writeFile/flush 不动）。
- 多实例才真正受益；单实例升级后行为与现在一致（锁语义不变）。
- 残余风险：进程崩溃在「写行」与「rename」之间丢失一条锁（窗口极小，容忍）；
  文件锁不跨主机（需要共享存储时才跨实例，NFS 原子性另议——P3 边界外）。
