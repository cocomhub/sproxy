# REPORT —— tun/tap 内核 VPN P1（roadmap 11.1-⑤）

## 状态

**DONE**（commit a99120794 + 22a1bbcb4；PR #607 已创建，CI 进行中）

## 交付

roadmap **11.1-⑤ tun/tap 内核 VPN P1 片**：接口 + 平台探测 + Linux 骨架 + Router 路由核心 + CLI 装配（**不做真设备功能**，真设备打开留 P2、真设备 e2e 留 P3）。

- **`pkg/vpn`**（新包，9 文件）：
  - `TUNDevice` 接口（Open/SetMTU/Addr）+ build tag 隔离实现：`device_linux.go`（tun 骨架，探测 `/dev/net/tun`）/ `device_windows.go`（wintun 标注）/ 未支持平台 fail-closed stub（`device_default.go`）
  - `PlatformProbe` 三态（支持/不支持/无特权）；`SetProbeTUNForTest` 测试注入点
  - `Router` 主循环：读 IP 包 → `vipTable.NodeByAddr` 选路 → mesh dial → 写包；未知 VIP / 拨号失败丢包不崩循环（per-conn 失败处理与 meshForwardListen 同构）；ctx 取消 / EOF 正常退出
- **`cmd/sclient/mesh_up.go`**：`--tun`/`--vip` flag（默认关零回归）+ `runMeshUpTUN` 装配（探测 → VIP 解析/子网校验 → hub 节点列表构建 vipTable（R-5 fail-closed：未分配明确报错）→ webrtc 信令 → mesh dial 链（smart/gateway/VIP 解析）→ TUNDevice + Router）
- **`cmd/sclient/mesh_up_tun_test.go`**：装配面测试（flag 注册 / 探测失败 fail-closed / 缺 VIP / 非法 VIP）

## 改动文件

| 文件 | 内容 |
|------|------|
| `pkg/vpn/device.go`（新） | TUNDevice 接口 + PlatformProbe + 哨兵错误 + 测试注入点 |
| `pkg/vpn/device_linux.go` / `device_windows.go` / `device_default.go`（新） | 平台 build tag 实现（Linux tun 骨架 / Windows wintun 标注 / 未支持 stub） |
| `pkg/vpn/router.go` + `router_test.go`（新） | Router 主循环 + 3 测试（未知 VIP 丢弃 / dial 失败不崩 / EOF 退出） |
| `pkg/vpn/device_test.go`（新） | 接口契约 + Probe 三态 + Open fail-closed 变异守卫 |
| `pkg/vpn/memory_tun.go` / `platform_unsupported.go`（新） | P1 内存兜底设备 / 平台矩阵记录 |
| `cmd/sclient/mesh_up.go`（+207/−17） | --tun/--vip 装配（fail-closed） |
| `cmd/sclient/mesh_up_tun_test.go`（新） | 装配面测试 |
| `docs/roadmap.md` | 11.1-⑤ → 已落地（P1）；8.3 残余注明 P2/P3 |

## 验证证据

- `go build ./...` + `GOOS=windows go build ./pkg/vpn/` 通过（windows build tag 隔离不破坏构建）
- `go test -count=1 -race ./pkg/vpn/ ./cmd/sclient/` 全绿（pkg/vpn 2.8s / cmd/sclient 12s）
- `go test ./internal/archcheck/` 全绿（24.5s）
- `go test -count=1 -race ./pkg/...` 全绿（无失败输出）
- golangci-lint 0 issues；gofmt / goimports 干净（router.go CRLF→LF 已统一，UTF-8 无 BOM）

## 收尾修正（主 agent 自查遗留）

1. **sclient 误产物二进制**：已删除，未入暂存区（`git add` 只含本任务文件）
2. **golangci-lint unused 4 处**：`unsupportedTUN`（device.go 的定义，仅未支持平台 tag 下使用）在 linux/windows 构建下被 unused 门禁拦截 → 定义移至 `device_default.go`（与使用方同 tag），lint 0
3. **router.go CRLF → LF**：统一 UTF-8 无 BOM 行尾

## 后续片

- **P2**：Linux ioctl TUNSETIFF 真设备打开 + MTU/地址装配；Windows wintun.dll 加载
- **P3**：真设备 e2e（对端回复链读 → 写 tun）

## PR

https://github.com/cocomhub/sproxy/pull/607
