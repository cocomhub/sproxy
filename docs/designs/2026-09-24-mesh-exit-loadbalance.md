# 设计：多出口负载均衡（11.1-④）

## 背景/目标
- 现状：`NewExitGroupDial`（pkg/tunnel/mesh/exit_route.go）for 循环**按序 failover**——首节点永远吃满流量，故障才切下一个，无轮询/加权；多出口带宽闲置。
- 目标：出口节点组补 `round-robin | weighted | failover` 三模式，`--exit-group-mode` 选择；**默认 failover 零回归**。模式选择器为纯函数（可单测 + 变异）。

## 组件与接口
- `pkg/tunnel/mesh/exit_route.go`：
  - 常量/枚举：`type ExitGroupMode string`；`const (ExitGroupFailover ExitGroupMode = "failover"; ExitGroupRoundRobin = "round-robin"; ExitGroupWeighted = "weighted")`。
  - `NewExitGroupDial` 签名扩展（**兼容**）：新增参数 `mode ExitGroupMode, weights []int`；**不改旧签名**——新增 `NewExitGroupDialWithMode(localTimeout, nodes, exitDialFor, mode, weights)`，旧函数委托之（mode=failover, weights=nil）→ 现有调用方（meshconn.AutoDial）零改动，编译期保证。
  - 新增纯函数：
    - `func PickExitGroup(mode ExitGroupMode, weights []int, counter *uint64, nodes []string) (int, error)`：failover → 0；round-robin → `int(atomic.AddUint64(counter, 1)-1) % len(nodes)`；weighted → 权重累加随机/轮转（实现：按权重扇区轮转 counter % total，确定性可测，避免 rand 不可测）。`len(nodes)==0` → error；weights 长度/全零 → 回落等权（文档化）。
    - `func NormalizeExitGroupMode(s string) (ExitGroupMode, error)`：未知值 → 错误（fail-closed，CLI 校验用）。
  - `NewExitGroupDialWithMode` 内部：**每连接**用 atomic counter 选起始节点，从该节点开始**循环 failover**（选中节点失败 → 尝试组内其余节点，覆盖全组）——round-robin 分发 + failover 兜底双语义。weighted 同构（counter 按权重扇区）。
- `cmd/sclient/internal/meshconn/meshconn.go`：
  - `Conn` 新增 `ExitGroupMode string`、`ExitGroupWeights []int`（可选）。
  - `AddExitFlags` 新增 `--exit-group-mode`（默认 `failover`）+ `--exit-group-weight`（StringSlice "node=weight" 或位置对应 []int；本期采用 `--exit-group-weight node-id:weight,node-id:weight` 显式命名防错位）。
  - `FromFlags` 校验：`NormalizeExitGroupMode` 非法 → 报错；weight 仅配合 weighted 使用（非 weighted 时忽略或报错——报错更 fail-closed）；weighted 且权重缺失 → 等权回落 + Warn（可观测）。
  - `AutoDial` 的 `--exit-group` 分支改调 `NewExitGroupDialWithMode`（传 mode/weights）。
- 错误传播：组内全部节点失败 → 原错误语义（fail-closed，最后一个错误）。

## 数据流
每连接拨号 → AutoDial → NewExitGroupDialWithMode → 闭包持 atomic counter：
- 选起点（PickExitGroup：failover=0 / round-robin=自增取模 / weighted=权重扇区自增）；
- 从起点按序尝试，失败循环试组内其余（**无论模式都保留 failover 兜底**，保证模式切换不牺牲可用性）；
- 成功返回 conn；全失败错误传播。
模式切换只影响**起点选择**，不改变失败恢复路径。

## 错误处理
- 空组 / exitDialFor nil：既有错误路径不变。
- 未知 mode / 非法 weight 语法：CLI 启动 fail-closed 报错（FromFlags 期拦截）。
- weighted 等权回落：Warn 日志（不静默）。
- 并发安全：atomic counter（race 安全；与既有 raceDial 并发语义一致）。

## 测试 + 变异点
- `TestPickExitGroup_Failover`：恒 0。**变异**：failover 也自增 → 红。
- `TestPickExitGroup_RoundRobin`：连续调用 0,1,2,0…（counter 注入）。**变异**：取模方向/起点错 → 红。
- `TestPickExitGroup_Weighted`：weights [3,1] → 扇区 0×3, 1×1 循环。**变异**：等权/权重错位 → 红。
- `TestPickExitGroup_EmptyNodes`：空组 error。**变异**：空组返回 0 → 红。
- `TestNormalizeExitGroupMode`：合法/非法/大小写。**变异**：未知值默认 failover（静默）→ 红。
- `TestExitGroupDialWithMode_RoundRobinDistributes`：mock exitDialFor 记录被调节点，N 连接断言分发序列。**变异**：闭包不用 atomic counter（每连接新建）→ 恒 0 → 红。
- `TestExitGroupDialWithMode_AllModesFailoverFallback`：首节点失败 → 循环尝试其余成功。**变异**：round-robin 模式失败后不循环（直接错误）→ 红。
- `TestExitGroupDialWithMode_DefaultModeFailover`（旧签名委托）：行为与现状一致（恒首节点）。**变异**：委托模式写错 → 红。
- FromFlags：`--exit-group-mode` 非法值/weight 语法/非 weighted 带 weight 报错。**变异**：校验被删 → 红。

## 片划分
- 片 1：pkg/tunnel/mesh 枚举 + PickExitGroup/Normalize + 新构造器（旧签名委托）+ 单测。
- 片 2：meshconn flags + 校验 + AutoDial 接线 + 测试。
- 片 3：文档（docs/cli.md --exit-group-mode/--exit-group-weight）+ R15 门禁同步。

## 风险与零回归保证
- 默认 `failover`：PickExitGroup 恒 0 = 原按序行为，逐字节兼容。
- 旧签名 `NewExitGroupDial` 保留委托，meshconn/测试/任何现有调用方零改动（编译期强制）。
- 每连接 atomic counter 开销可忽略；race 安全。
- 风险：weighted 的确定性扇区轮转非「随机加权」——对长连接分布等效、可测性优先；文档明示（如需随机加权后续片换 rand 源，接口不变）。
