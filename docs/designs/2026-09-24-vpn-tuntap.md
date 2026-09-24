# tun/tap 内核 VPN（11.1-⑤，长期）

## 背景 / 目标
- 现状：`mesh up` 是用户态 SOCKS5（cmd/sclient/mesh.go，无内核特权）；`mesh connect` 已支持虚拟 IP 寻址（`mesh.VipTable`：`NewVipTable`/`Add`/`NodeByAddr`/`ParseVirtualAddr`/`IsVirtualAddr`，数据源 = hub 权威节点列表 `ListHubNodes` 的 `VirtualIP` 字段）。
- 目标：tun/tap 内核虚拟网卡，整网段透明路由（IP 包按目标 VIP 选路，无需逐连接转发）；虚拟 IP 分配复用 hub 权威 VipTable；**标注长期 + 平台分片**（Linux tun / Windows wintun），本期只做接口设计与平台探测，不做真设备功能。

## 组件与接口
- `pkg/vpn/`（新包，领域层，G1 叶子）：
  - `TUNDevice` 接口：`Open(name) (io.ReadWriteCloser, error)` / `SetMTU(int) error` / `Addr() netip.Addr`；平台实现经 build tag 隔离（`device_linux.go` / `device_windows.go`），未支持平台不参与构建。
  - `Router`：`NewRouter(dev io.ReadWriteCloser, dial meshDialFunc, vip *mesh.VipTable, subnet netip.Prefix)`；`Serve(ctx) error` 主循环：读 IP 包 → 解析 dst → `vipTable.NodeByAddr` → dial 建链 → 写包；对端回复链读 → 写 tun。
  - `PlatformProbe() (supported bool, err error)`：无特权 / 设备不可用时的探测入口（本期可单测的唯一行为）。
- CLI：`mesh up --tun --vip <addr>`（或 `--auto-vip` 经 `ListHubNodes` 发现本机已分配 VIP）。dial 装配复用 mesh connect 既有链路：`meshDialFunc` 组合（`mesh.Dial` → `--smart` → `--gateway` → `--e2e` 包层顺序不变）。

## 数据流
1. 管理员特权启动 `mesh up --tun --vip 100.64.0.5` → `PlatformProbe` 通过 → `TUNDevice.Open` + `SetMTU(1400)`（VPN 隧道 MTU，防分片）。
2. 拉 `ListHubNodes` 构建 `vipTable`（与 mesh connect 同源、同冲突 fail-closed 语义）。
3. 应用发往 `100.64.0.7:22` 的包 → tun 读 → `NodeByAddr(vip)` 解析 node-id → dial（webrtc 直连 / hub 中继，含 E2E 包层）→ 隧道内透传 IP 包。
4. 对端节点在服务侧收到隧道数据后由目标服务回包 → 反向写回 tun → 应用栈完成 TCP 会话。

## 错误处理
- `PlatformProbe` 失败 / `Open` EPERM → 明确报错（提示 sudo/管理员、wintun 驱动缺失），**不静默回落 SOCKS5**（禁静默降级）。
- VIP 未分配 / 不在列表 → fail-closed 报错（复用 mesh connect R-5 语义：提示重试/确认 hub 已分配）。
- MTU 与实际隧道不符 → 启动日志 warn + 文档建议值，不强制。
- 单包 dial 失败 → 丢弃该包 + 计数日志（不崩循环），与 `meshForwardListen` 的 per-conn 失败处理同构。

## 测试 + 变异点（本期仅接口 + 平台探测）
- `TestTUNDeviceInterface`：三平台 build tag 编译 + `PlatformProbe` 桩行为（支持/不支持/无特权三态）。
- `TestRouter_DstNotInVipTable`：`NodeByAddr` 未命中 → 包被丢弃（变异：把包继续写 tun → 红）。
- `TestRouter_DialFail`：dial 失败 → 包丢弃不 panic（变异：panic/重试死循环 → 红）。
- 真设备 e2e 标注为**后续片**（CI 无特权环境），不冒充已验证。

## 片划分
- P1：`pkg/vpn` 接口 + `PlatformProbe` + Linux tun 实现骨架 + Router 路由逻辑（纯函数可单测）+ CLI `--tun` 装配。
- P2：Windows wintun（x/sys/windows + wintun.dll 加载）。
- P3：真设备 e2e（特权环境）+ 文档 + MTU/多子网调优。

## 风险与零回归
- 新 `--tun` flag 默认关；`mesh up` 既有 SOCKS5 路径零改动（默认行为不变）。
- 平台 build tag 隔离：不支持平台不引入编译依赖。
- 特权要求、驱动依赖、wintun 许可证全部文档化，作为长期特性明示。
- 不与 `mesh connect` 的 VipTable 实现分叉：复用同一 hub 权威数据源。
