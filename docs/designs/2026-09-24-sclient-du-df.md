# 设计：sclient du/df 空间统计（11.7-①）

## 背景与目标
- 现状：服务端 GET /api/stats 已含 DiskUsage/Storage* 分类/Disk* 与 per-owner Quota
  （pkg/server/stats.go：statsHandler、statsRootFor、walkUploadStats*、quotaStatusOf）；
  sclient 侧仅有 NewCmdStats 聚合展示（root.go 注册），无按目录的 du 与卷水位 df。
- 目标：
  1. `sclient du [path]`：按目录递归统计（文件数/字节/子目录数），默认当前目录，
     随 cd 与 --vol 生效；
  2. `sclient df`：卷水位输出（per-owner 使用量/上限/水位 + 磁盘总/已用/可用）。
- 零回归红线：只新增端点与命令，不改 /api/stats 既有 JSON 字段与语义。

## 组件与接口
- 服务端新增 GET /api/du?path=<rel>（pkg/server/du.go）：
  - 认证：走 authMiddleware（SproxySig/APIKey），owner 从 ActorFrom(r.Context()) 取；
  - 定位：locateForRead(owner, "user/"+rel, explicitVol) 定位卷+租户（ACL 收口），
    未命中 → 404（不泄卷存在性）；
  - 遍历：复用 stats 桶语义（跳过 cloud/chunk/version/meta/. __ 魔法目录/
    checksums.json/LAYOUT_VERSION），仅统计 user 桶内子树；
  - 响应：`{"success":true,"data":{"path","dirs","files","size"}}`；非法路径（穿越/
    绝对路径/空字节）由 ValidateFilePath 拒绝 400。
- df：复用 /api/stats 现有字段（零新增端点）——Quota{usage,max_bytes,watermark}
  + storage_* + disk_*。
- pkg/client：FileClient 新增 `Du(ctx, path string) (*DuResult, error)`；Stats 已有。
- cmd/sclient：新增 du.go/df.go，模式对齐 root.go 既有工厂（factory+ios+cliState）；
  输出人类可读大小（本地 helper，如 ls -lh），--json 走现有持久 flag。

## 数据流
1. `sclient du <path>` → FileClient.Du → GET /api/du?path=...（SproxySig 签名）→
   locateForRead 定位（默认卷快路径/视图遍历）→ WalkDir 统计 → JSON → 表格/JSON 输出；
2. `sclient df` → FileClient.Stats → GET /api/stats → 解析 Quota/Disk/Storage 字段
   → 表格式输出（owner 视图）或汇总（admin 空 owner）。

## 错误处理
- du：400 非法路径；401/403 未认证/卷 ACL 排除（404 统一，防探测）；遍历 IO 错误
  （WalkDir err 记 Warn 且返回 success:false——不得把部分结果当完整结果，fail-closed）；
- df：/api/stats 返回 503（存储统计未完成首次扫描，statsHandler 现状）→ 友好提示
  稍后重试；
- 网络错误沿用 pkg/client 既有错误链，--verbose 输出细节。

## 测试与变异点
- 服务端 du：fixture 树（嵌套目录 + cloud/chunk/version/meta 桶 + . __ 目录 + 旧布局
  平铺文件）断言 dirs/files/size 精确；变异：桶跳过逻辑删除/反转 → 红；
- 越权：owner A 请求路径指向 owner B 内容 → 404/空结果（ACL 收口断言）；
- 客户端输出：人类可读格式化表驱动（0/1023/1MiB 边界）；变异：单位换算错 → 红；
- df 水位：quotaStatusOf 边界（0%、50%、>100%——现状不封顶，加断言防意外变更）；
  变异：watermark 计算移除 → 红。

## 片划分
- P1：服务端 /api/du + 单测（桶判定抽共享 helper，与 stats 单一事实源）；
- P2：pkg/client Du + mock server 契约测试（pkg/testutil/mockserver 扩展）；
- P3：cmd/sclient du.go/df.go + 输出纯函数表驱动测试（go test，无 HTTP 依赖）；
- P4：docs/cli.md 登记 du/df 与 /api/du（R15 文档漂移门禁覆盖）。

## 风险与零回归保证
- 遍历语义与 stats 双实现漂移风险：抽共享 bucket 判定 helper，变异点防漂移；
- 新端点暴露面：只返回聚合数字（无文件名/内容），ACL 经 locateForRead 收口；
  默认卷排除面下 du 对排除 owner 404（属 user 文件面，区别于 stats 聚合例外）；
- 零回归：stats.go 字段与语义不动；既有命令行为不变；新端点默认无影响。
