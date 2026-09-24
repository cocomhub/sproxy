# 设计：卷操作 CLI（11.7-④）

## 背景与目标
- 现状：服务端已有 POST /api/volumes/{copy,move,rebalance}（任务简报为准；实现位置
  经文件服务抽取后可能已下沉 pkg/files 或 handlers_*.go——实施第一步先定位核对，
  勿重复实现）；sclient volume 命令仅 create/list/delete（root.go 注册
  NewCmdVolumes/NewCmdVolume）。
- 目标：`sclient volume copy <from> <to> [--vol]` / `move <from> <to> [--vol]` /
  `rebalance`，把卷间文件操作与再平衡放进 CLI，供脚本化与人工运维。
- 零回归：只加子命令与客户端调用，不改服务端既有端点行为。

## 组件与接口
- cmd/sclient/volume_ops.go：在 volume 命令下挂 copy/move/rebalance 子命令；复用
  root --vol（卷上下文，PersistentPreRunE 已绑定）与 cd 当前目录语义；
- pkg/client：FileClient 新增 VolumeCopy(ctx, from, to, vol) / VolumeMove(ctx, from,
  to, vol) / VolumeRebalance(ctx)——内部 POST /api/volumes/{copy,move,rebalance}，
  参数编码对齐服务端 handler 实际签名（实施时核对）；
- 输出：copy/move 打印目标位置与校验；rebalance 打印任务已提交（异步语义）。

## 数据流
1. copy/move：CLI 解析 from/to（cd 拼接 + ValidateFilePath 客户端预检）→ 签名请求 →
   服务端定位源卷（locateForRead 语义）→ 复制/移动（卷内或跨卷）→ 双账本结算
   （scope+pool TryReserve/Commit，复用 routeUpload 语义）→ {success,message}；
2. rebalance：CLI POST → 服务端触发再平衡任务（异步）→ 立即返回已提交；进度后续经
   GET /api/volumes 或日志观察。

## 错误处理
- 400 非法路径；404 源文件不存在/卷不可见（统一 404，不泄卷存在性）；
- 409 目标已存在（copy/move 冲突，保守默认：不覆盖，报冲突；显式覆盖另议）；
- 403 卷 ACL 排除；507 配额/容量不足（映射 routeErrOwnerFull/routeErrVolFull）；
- rebalance 非 admin → 403；网络/超时沿用 pkg/client 错误链，--verbose 诊断。

## 测试与变异点
- copy 成功：mock 服务端（pkg/testutil/mockserver 扩展 /api/volumes/copy）断言请求
  URL/参数/成功解析；变异：去掉 POST 调用或参数名错 → 红；
- move 冲突：409 路径断言友好错误文案（表驱动）；
- rebalance 提交：断言 POST 目标路径与 body、202/200 处理；变异：不调用 rebalance
  端点（假成功）→ 红（fail-closed，对齐 via-direct 假成功教训）；
- CLI 参数预检：穿越路径（../）本地即拒（ValidateFilePath 复用）；卷名未知 → 403/404。

## 片划分
- P1：服务端 handler 定位核对（存在仅核对契约；缺失才补最小实现 + 单测：copy 复制
  文件+checksum 台账、move 跨卷搬迁+双账本、rebalance 触发）；
- P2：pkg/client 三个方法 + mock 契约测试；
- P3：CLI 子命令 + 输出格式化（表驱动纯函数）；
- P4：docs/cli.md + volume 命令帮助补全（R15）。

## 风险与零回归保证
- 简报所述 mirrorVolume/rebalanceVolumeHandler 与本次读取的 volumes.go 现状不符
  （未见实现）：实施第一步全局定位（pkg/server、pkg/files），找不到按「补最小
  handler」处理——已记录，属实施前核对项；
- 客户端先行、服务端仅核对/补缺：既有端点行为零改动；
- 卷唯一性/覆盖写语义（AD-4/F1 stay-home）不得被 CLI 绕过：move 目标冲突必须 409，
  绝不在 CLI 层静默覆盖，防跨卷双份 + owner 双计。
