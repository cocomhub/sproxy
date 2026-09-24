# 设计：卷级数据保留策略（11.7-⑨）

## 背景与目标
- 现状：VolumeConfig（pkg/server/config.go）无 retention；审计 ring（audit.buffer_
  size）、分享 expire_in、版本 retention（versioning.retention）、回收站 TTL 各自
  独立、全局配置。
- 目标：`volumes[].retention` 卷级统一保留策略：对绑定本卷的过期数据（版本桶、
  分享 token、审计日志）做周期清理；0/缺省 = 关闭，零回归。
- 设计边界：审计为**默认卷单点权威**（<默认卷根>/audit/audit.log + 内存 ring）——
  retention.audit_ttl 仅默认卷生效；其它卷配置该键 → 装配期 Warn（禁静默忽略）。

## 组件与接口
- 配置（config.go）：VolumeConfig 增 `Retention VolumeRetentionConfig`：
  - VersionTTL time.Duration（0=不启用版本龄清理；>0 覆盖全局 versioning.retention
    于该卷）；
  - ShareTTL time.Duration（0=不启用；>0 卷级分享过期兜底）；
  - AuditTTL time.Duration（仅默认卷；0=不启用）；
  - GCInterval time.Duration（0=关闭周期 GC；>0 启动卷级清理 ticker）；
  - Validate：负值拒绝；GCInterval>0 且全 TTL 为 0 → 拒绝（无意义的空转任务）。
- 装配（volumes.go）：assembleVolumes 把 Retention 透传进 volume.Volume（新字段）；
  服务端启动卷级 GC goroutine（ticker+stop channel+WaitGroup，对齐 mirror_interval/
  tier_policy 既有模式）；卷关闭时停止。
- 清理执行器（pkg/volume/retention 或 server 内）：
  - 版本：遍历 <卷根>/<owner>/version/，超龄即删（复用版本删除原语/台账）；
  - 分享：扫描 <卷根>/anonymous/meta/share/*.json，expire_at 过期即删（原子删，
    复用分享持久化删除路径）；
  - 审计（仅默认卷）：audit.log 按龄截断/滚动（保留最近 AuditTTL 窗口）。

## 数据流
1. 配置载入 → Validate → assembleVolumes 装配 volume.Volume.Retention；
2. 启动 GC：每 GCInterval 对每个启用卷执行一次清理（逐 owner 遍历版本/分享 meta）；
3. 删除动作复用领域原语（同一 checksum/台账/双账本释放路径），不留孤儿。

## 错误处理
- 单文件清理失败（权限/IO）：记 Warn 继续（清理是尽力而为的运维任务，不阻断）；
- 卷根不可达：跳过该卷并 Warn（fail-closed：不清 = 安全方向，不误删）；
- Validate 非法配置启动即失败（响亮拒绝，防「配了没用」静默失效）。

## 测试与变异点
- retention 过期清理（核心变异点）：fixture 卷放过期/未过期版本文件 + 分享 token，
  手动触发一次 GC → 断言过期删除、未过期保留、台账一致；变异：龄比较方向反转/
  漏删分享类目 → 红；
- 关闭语义：GCInterval=0 → 不启动 goroutine（断言无任何清理发生，零回归）；
- 卷级覆盖：卷级 VersionTTL 生效时全局 versioning.retention 不影响该卷（优先级）；
- 非默认卷 AuditTTL → 装配 Warn（日志断言）；
- 龄判定纯函数（now 注入）表驱动。

## 片划分
- P1：配置字段 + Validate + 装配透传 + 龄判定纯函数（TDD，变异）；
- P2：版本/分享清理执行器（复用领域原语）+ 测试直调触发；
- P3：周期 GC goroutine + 生命周期接线（停止/重启）+ 装配 Warn；
- P4：文档（config.md volumes 段）+ 门禁核对（R12/装配一致）。

## 风险与零回归保证
- 与既有三套 TTL 语义并存：卷级优先、全局兜底（文档注明）；默认零值全关闭 = 与
  现状完全一致；审计仅默认卷（单点权威不变）；
- 清理与读写并发：删除走既有锁（FileLocks/版本删除原语），GC 不与上传/下载互踩；
- 零回归：全 TTL=0 时装配路径不启动任何新 goroutine，既有行为逐位不变。
