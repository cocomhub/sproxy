# 设计：迁移向导（11.7-⑩）

## 背景与目标
- 现状：无 migrate 工具；跨机搬迁只能手动 download/upload，多卷/联邦配置需手工编辑
  YAML（volumes[].mirror_to、sync_remotes、federation）。
- 目标：`sclient migrate` 向导：单机 → 多卷 → 联邦的自动化迁移——导出（清单+文件）
  → 导入（目标机校验还原）→ 镜像/联邦配置生成；交互与纯脚本双模式。
- 零回归：纯新增命令与包 + 复用既有 list/download/upload；服务端零改动（可选只读
  端点按片 P1 决策）。

## 组件与接口
- cmd/sclient/migrate.go：cobra 命令，子命令 export/import/mirror-config；flags：
  --yes（跳过交互）、--export-only/--import-only/--no-verify、--ignore-errors、
  --out/--in（目录）。
- pkg/migrate（新包，纯逻辑可测）：
  - Manifest：`{schema:1, server:{url}, exported_at, files:[{name,size,checksum,
    mtime,volume?}]}`；
  - Exporter：递归 list（/api/files + subdir 遍历）→ 下载到 <out>/files/（相对路径
    保持）→ 校验 checksum → 原子写 manifest.json；
  - Importer：读 manifest → 逐文件上传（复用 FileClient.Upload/分块/续传）→ 目标
    stat/checksum 校验；同名同 checksum 跳过（幂等）；
  - MirrorConfig：由 manifest + 目标机卷列表（GET /api/volumes）生成 volumes[].
    mirror_to / sync_remotes / federation 片段 YAML（--print 或 --write）。

## 数据流
1. export：源 server 认证（context/config 解析，复用 resolvedContext）→ 递归列出
   全部文件（含卷上下文）→ 下载到 <out>/files/ → 写 manifest.json → 摘要打印
   （N 文件/B 字节）；
2. import：目标 server → 读 manifest（schema/checksum 自检）→ 上传全部文件 →
   逐文件校验（目标 checksum == manifest checksum）→ 汇总报告；失败清单收集，
   --ignore-errors 才继续，否则非零退出；
3. mirror-config：读 manifest + 目标机卷列表 → 生成 YAML 片段（dry-run 打印/
   --write 落盘）→ 提示合并进目标配置后重启生效。

## 错误处理
- manifest 缺失/损坏/schema 不符：拒绝导入（fail-closed，防静默错迁）；
- 单文件下载/上传失败：记入失败清单（含原因），默认中止后续（快速失败），
  --ignore-errors 跳过继续；
- checksum 校验失败：该文件标 FAILED，绝不上报成功（假成功红线，对齐 via-direct
  教训）；目标已存在且 checksum 相同 → SKIPPED（幂等）；不同 → CONFLICT 报错；
- 源/目标不可达：预检（stats/version 探活）先失败再开跑。

## 测试与变异点
- 迁移步骤序列（核心变异点）：双 mock server（源/目标）跑 export→import 全流程，
  断言步骤顺序（清单→下载→上传→校验）与文件数/字节一致；变异：调换步骤/跳过
  校验/校验不比对 checksum → 红；
- manifest round-trip：序列化/反序列化一致性；篡改 checksum 后 import 拒绝；
- 幂等：二次 import 全 SKIPPED（同 checksum 跳过）；
- 冲突：目标同名不同 checksum → CONFLICT 且非零退出；
- mirror-config 生成：给定 manifest+卷列表，断言 YAML 片段含 mirror_to/sync_remotes
  键（结构断言）；变异：漏生成 mirror_to → 红。

## 片划分
- P1：pkg/migrate Manifest + 纯函数（序列化/校验/冲突分类）TDD；
- P2：Exporter/Importer（复用 FileClient）+ 双 mock server 端到端单测（127.0.0.1）；
- P3：mirror-config 生成器 + CLI 子命令接线（交互提示走 cli.Ask 或简单 stdin 扫描）；
- P4：docs/cli.md + 迁移手册示例（多卷/联邦目标配置样例）。

## 风险与零回归保证
- 大数据量迁移耗时长：分块上传/续传复用既有能力；进度按文件计数输出；manifest
  支持断点重导（已 SKIPPED 文件直接跳过）；
- 迁移期间源端变化：manifest 记录 mtime/checksum，导入后校验发现差异 → 提示重导
  该文件（幂等重跑即可收敛）；
- 零回归：纯新增命令与包；服务端零改动（除片 P1 可选只读端点）；既有 upload/
  download/list 行为不动。
