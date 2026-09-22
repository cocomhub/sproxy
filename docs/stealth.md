# 被动伪装层：JA3 指纹差异清单与检测方法

> roadmap §5.3 P1（跨墙可识别性）：不引入新混淆算法，做「形态对齐」——TLS 握手参数贴近主流
> HTTP 栈。本专节是 DPI 特征检测报告（JA3 指纹差异清单）与验证方法。
>
> 配套配置（默认全关，显式启用；启用状态日志可见，禁静默降级）：
> - `tls.cipher_order` / `tls.alpn`（TLS 形态对齐，#453）
> - `tunnel.idle_padding`（空闲填充，#453）
> - `hub.transports.ws.path` / `hub.transports.ws.upgrade_header`（WS 形态对齐，#459）

## 1. JA3 指纹是什么

JA3 是 TLS ClientHello 的哈希指纹：按 `TLS 版本 | cipher 顺序 | 扩展列表 | 椭圆曲线 | 椭圆曲线点格式`
拼接后 MD5。DPI 设备用 JA3 识别「这是 Go 自签服务 / 自定义客户端」——与主流浏览器/HTTP 栈指纹不同
即暴露。

## 2. 现状 vs 主流 HTTP 栈差异清单

sproxy 未启用伪装时的默认 TLS 行为（Go 1.27 crypto/tls 默认）：

| 维度 | sproxy 现状（Go 默认） | 主流 HTTP 栈（Chrome/curl/nginx） | 差异影响 |
|------|------------------------|-----------------------------------|----------|
| TLS 版本 | TLS 1.3（+1.2 兼容） | TLS 1.3（+1.2 兼容） | 低（两者都有 1.3） |
| cipher 顺序 | Go 默认序（`TLS_AES_128_GCM_SHA256` 优先，1.2 段含 `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256` 等） | Chrome 优先 `TLS_AES_128_GCM_SHA256`/`TLS_CHACHA20_POLY1305_SHA256` 混合序；1.2 段含 `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256` 等 | **中**：1.2 cipher 集合与顺序不同 → JA3 可区分 |
| ALPN | 服务端按 `NextProtos`（sproxy 未显式配置时无/有限） | Chrome 发 `h2, http/1.1`；nginx 服务端接 h2 | **中**：无 ALPN 或单 http/1.1 与主流 h2 不同 |
| 扩展顺序 | Go 固定序（`supported_versions` 后跟 `supported_groups` 等） | Chrome 独特序（`supported_groups` 前置等） | **中**：扩展顺序是 JA3 重要输入 |
| 椭圆曲线 | Go 默认 `X25519` 优先 + P-256/P-384 | Chrome `X25519` 优先 + P-256 | 低（X25519 都在前） |
| 证书 | 自签 ECDSA P-256（`tls.auto_tls`） | CA 签发的 RSA/ECDSA 主流证书 | **高**：证书来源是 DPI 首要判据（自签 = 非主流服务） |
| TLS 扩展 | 无 `extended_master_secret` 重协商等差异 | 标准扩展集 | 低-中 |

## 3. 启用伪装后的收敛项

| 配置 | 收敛的差异 | 说明 |
|------|-----------|------|
| `tls.cipher_order: [TLS_AES_128_GCM_SHA256 TLS_AES_256_GCM_SHA384 TLS_CHACHA20_POLY1305_SHA256 ...]` | cipher 顺序贴近 Chrome 1.3 段 | 1.2 段也按主流序配 |
| `tls.alpn: [h2 http/1.1]` | ALPN 与主流 HTTP/2 栈一致 | 服务端宣告 h2 → 客户端协商一致 |
| CDN 前置（Cloudflare/自建 Nginx 终结 TLS） | **证书来源差异彻底消除**（CA 证书 + 标准指纹） | 首选：CDN 终结 TLS 后回源 WS/TCP；sproxy 自身证书不出网 |
| `hub.transports.ws.path: /api/v1/stream` | WS 端点不再暴露 `/ws` 特征路径 | 贴近业务路径形态 |
| `hub.transports.ws.upgrade_header: <值>` | 升级握手带自定义指纹头（可选） | 两端一致才连通 |

**注意**：`tls.cipher_order`/`tls.alpn` 只调整服务端宣告的 cipher/ALPN——**完整收敛 JA3 仍需 CDN
前置**（服务端 ClientHello 指纹由客户端决定；服务端指纹收敛点在于证书与支持列表）。对
`sclient` 客户端出网：走 CDN 时其 TLS 由 CDN 终结，sproxy 自定义指纹不出网。

## 4. 检测方法（验证收敛）

```bash
# 抓取 sproxy 服务端 ClientHello → JA3
tshark -r capture.pcap -Y "tls.handshake.type==1" -T fields -e tls.handshake.ja3 -e tls.handshake.ja3_full

# 或 curl 探测 + ja3 计算（工具：saleor/ja3 / jvns 脚本）
curl -sv --tlsv1.2 https://<sproxy-host>/ 2>&1 | grep -i "cipher\|alpn"

# 对比参考：抓 chrome/curl 到同一目标的 ClientHello JA3
# 收敛判定：启用伪装配置后两次 JA3 的 cipher/ALPN 段一致（证书段差异由 CDN 消除）

# WS 路径探测（确认 /ws 特征已隐藏）
curl -sv https://<sproxy-host>/ws 2>&1 | grep -i "404\|upgrade"   # 自定义后 404
curl -sv https://<sproxy-host>/api/v1/stream 2>&1 | grep -i "101\|upgrade"
```

## 5. 残余差异与说明

- **扩展顺序**（supported_groups 等）Go 客户端无法完全模拟 Chrome——需 CDN 终结才能隐藏；
  服务端自身（sproxy listen）指纹由客户端决定，服务端 JA3 意义有限。
- **证书来源**：自签证书若出网（非 CDN 前置）仍可被识别——建议生产一律 CDN/正式证书前置。
- 本清单随 Go 版本与 Chrome 更新需定期复审（tls.cipher_order 列表与主流栈对齐）。
