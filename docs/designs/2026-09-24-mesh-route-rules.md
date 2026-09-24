# 设计：mesh 域名/网段分流（11.1-③）

## 背景/目标
- 现状：出口策略仅 `--exit-group` 按序 failover + `--exit-auto` 能力候选；`--dial-allow-cidr` 是出口侧目标放行白名单（非本机路由选择）。不同目标走不同出口（如内网 A 出口、公网 B 出口）无法表达。
- 目标：新增 `--route <domain|cidr>=<exit-group>` 分流规则：socks/udp/http-proxy/mesh connect 统一出口选择。规则命中目标 → 走指定出口组；无规则命中 → 回落现有默认（--exit/--exit-group/--exit-auto/本地直连）——**零回归**。

## 组件与接口
- `cmd/sclient/internal/meshconn/meshconn.go`：
  - `Conn` 新增 `Routes []RouteRule`。
  - `type RouteRule struct { Kind RouteKind; Pattern string; Group []string }`，`RouteKind` ∈ domain（后缀匹配）/ cidr（`netip.ParsePrefix`）。
  - `AddExitFlags` 新增 `f.StringSlice("route", nil, "分流规则 domain|cidr=exit-group（逗号分隔多规则；示例 --route .example.com=node-a,node-b --route 10.0.0.0/8=node-c；域名后缀匹配，cidr 前缀匹配；多规则按声明序首个命中；未命中回落默认出口）")`。
  - `FromFlags` 解析：`strings.Cut(rule, "=")`；非法格式/非法 CIDR → **启动即报错**（fail-closed，不静默忽略）；空 group → 报错。**域名规则归一化**：去首 `*`、前导 `.`，小写（`*.example.com`/`.example.com` 同义）；`netip.ParsePrefix` 失败即非法 CIDR。
- 出口选择核心（纯函数，放 meshconn 便于单测）：
  - `func (c *Conn) SelectRoute(addr string) []string`：解析 `addr` host（`net.SplitHostPort`）→ 域名走 suffix 匹配、IP 走 cidr 匹配 → 首个命中返回其 Group；无命中返回 nil。
- `AutoDial` 改造：
  - 拨号闭包内先 `SelectRoute(addr)`：命中 → `mesh.NewExitGroupDial(localTimeout, group, exitDialFor)`（本地直连优先 + 组内 failover，复用现有装配）；未命中 → 走原逻辑。
  - 注意：**退出组是 per-dial 构造**（与现有 --exit-group 一致），路由解析在每连接拨号时进行（目标 host 每连接可能不同）。
- 各命令接入：socks.go/http_proxy.go 已收敛于 `conn.AutoDial` → 一处改造全部生效；udp.go 是**固定单出口映射**（--remote 一个目标）：路由规则命中 remote host 时替换其出口组（`OpenUDPMux` 的 exit 节点从 SelectRoute(remote) 取，仍单 mux 固定出口）；mesh connect 无出口语义不接。
- 互斥校验（FromFlags）：--route 与 --exit-auto/--exit 可共存（route 是优先级更高的分流，未命中才用默认），**不禁用**；但 route 与 --exit-only 语义冲突 → 拒绝（--exit-only 恒经出口，分流无意义）——注释写明。

## 数据流
连接目标 addr（socks CONNECT host / http-proxy 绝对 URI host / udp --remote / cloud download URL host）→ AutoDial 闭包 → SelectRoute(addr)：
- 命中：`NewExitGroupDial(localTimeout, group, exitDialFor)`（本地直连优先 → 组内 failover → 全部失败错误传播）；
- 未命中：原逻辑（--exit / --exit-group / --exit-auto / 纯本地）。
cloud download 服务端侧（cfg.CloudDownloadExitNode）本期**不接**（配置层单值，无 CLI 路由表；列为后续片）。

## 错误处理
- 非法规则/CIDR/空组：命令启动 fail-closed 报错（列出具体规则文本）。
- 组内全失败：错误向上传播（与 --exit-group 相同 fail-closed，不吞错）。
- 域名解析失败（DNS 不解析为 IP 的 host）：域名规则按字符串后缀匹配（不依赖 DNS），cidr 规则对纯域名 host 不命中（回落默认）——注释明示边界。
- 多规则同优先级：首个命中即止（声明序，文档化）。

## 测试 + 变异点
- `TestSelectRoute_DomainSuffix`：`example.com` 命中 `.example.com`；`sub.example.com` 命中；`notexample.com` 不命中。**变异**：后缀匹配改 `strings.HasPrefix`/去掉规范化 → 红。
- `TestSelectRoute_CIDR`：`10.1.2.3:80` 命中 `10.0.0.0/8`；`192.168.x` 不命中；IPv6 cidr 命中。**变异**：cidr 用字符串前缀比较 → 红。
- `TestSelectRoute_OrderAndFallback`：多规则首命中；无命中返回 nil。**变异**：倒序/全命中 → 红。
- `TestFromFlags_RouteParse`：合法/非法格式/CIDR/空组用例。**变异**：解析错误被忽略 → 红。
- `TestAutoDial_RouteWins_ElseDefault`（mock exitDialFor）：目标命中路由组走 NewExitGroupDial（组内 failover 行为断言）；未命中走默认（--exit 节点）。**变异**：AutoDial 忽略 SelectRoute → 红。
- `TestUDPMap_RouteRemoteHost`：remote host 命中路由组替换出口节点。**变异**：udp 不接 SelectRoute → 红。

## 片划分
- 片 1：meshconn 数据结构 + FromFlags 解析 + SelectRoute 纯函数 + 单测。
- 片 2：AutoDial 接线 + socks/http-proxy 生效（天然收敛）+ 互斥校验 + 测试。
- 片 3：udp map 接入 + 文档（docs/cli.md --route 说明 + config 示例）+ 门禁 R15 文档漂移同步（新 flag 必须出现在 docs/cli.md）。

## 风险与零回归保证
- 新 flag 默认空：SelectRoute 返回 nil → AutoDial 走原路径（零回归）。
- 出口选择收敛在 meshconn 单点（AutoDial），四命令统一收益。
- 域名规则用后缀匹配避免 DNS 依赖与副作用。
- 风险：规则数量无上限——文档建议保持小规模；每连接 SelectRoute 是 O(rules) 线性开销（规则少，可忽略）。
