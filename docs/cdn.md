# CDN WebSocket 前置指南

> 目标：在强管制/严格防火墙网络下，让 sproxy 的 WebSocket 传输（`hub.transports.ws`）
> 稳定穿越 CDN 前置。按本指南配置可达到：TLS 指纹由 CDN 终结消除、WS 流量混入正常
> Web 流量形态、传输策略在劣化时自动切换（防抖 + 手动锁定）。

## 1. 为什么需要 CDN 前置

- **证书来源指纹**：自签证书的证书链/签发者与正规 CA 不同，DPI 可据此识别；
  自建 Nginx 前置并用 ACME 正式证书终结 TLS 后，证书来源指纹即被消除。
- **TLS 握手指纹**：sproxy 服务端的 ClientHello（cipher 顺序/ALPN）与主流 HTTP 栈
  有差异（见 [stealth.md](./stealth.md)）；CDN 终结后，客户端到 CDN 段的 TLS 由
  CDN 承担，sproxy 只见 CDN 的回源连接。
- **WS 特征隐藏**：`Upgrade: websocket` 升级头在穿越严格代理时可能被识别；
  CDN 的 WS 支持让升级在边缘完成，回源走普通 HTTP/WS 通道。
- **IP 隐藏**：回源域名后隐藏源站 IP，防 DDoS 直接打到源站。

## 2. 前置拓扑

```
客户端 (sclient relay --transport ws)
   │  WSS  (443, 标准 TLS + WS 升级)
   ▼
CDN 边缘（Cloudflare / 自建 Nginx + Certbot）
   │  WS 回源（HTTP/WS，或 WSS 到源站 443）
   ▼
sproxy 源站（hub.transports.ws.enabled=true）
   │
   ▼
mesh 节点（relay / 隧道数据面）
```

两种推荐形态：

| 形态 | 说明 | 适用 |
|------|------|------|
| **托管 CDN（Cloudflare 等）** | 边缘终结 TLS + WS 升级，回源 WS 到源站 | 有公网域名、需隐藏源站 IP |
| **自建 Nginx 反代** | `nginx` + `certbot` 终结 TLS，`proxy_pass` WS 到源站 | 自有服务器、需正式证书 |

## 3. 服务端配置

```yaml
# config.yaml
hub:
  enabled: true
  transports:
    ws:
      enabled: true
      listen: "127.0.0.1:18084"   # 回源监听（仅本机/内网）
      path: "/api/v1/stream"       # 形态对齐业务路径（默认 /ws 零回归）
      upgrade_header: "secret-fp"  # 附加校验头（防未授权升级；sclient 同步）
```

关键点：

- `listen` 绑 **127.0.0.1/内网**，公网流量只进 CDN/Nginx——源站不直接暴露 WS。
- `path` 自定义为业务路径（如 `/api/v1/stream`）——贴近正常 API 流量形态。
- `upgrade_header` 非空时服务端校验 `X-WebSocket-Profile` 头（不匹配 400），
  防 CDN 后未授权直连源站。
- 客户端（sclient relay）同步：`--transport ws --ws-path /api/v1/stream --ws-upgrade-header secret-fp`。

## 4. 自建 Nginx 反代示例

```nginx
# /etc/nginx/sites-available/sproxy
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl http2;
    server_name sproxy.example.com;

    ssl_certificate     /etc/letsencrypt/live/sproxy.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sproxy.example.com/privkey.pem;

    # WS 回源（sproxy 本地 WS 监听）
    location /api/v1/stream {
        proxy_pass http://127.0.0.1:18084;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_read_timeout 120s;   # 长连接 WS：超过心跳 90s 上限
        proxy_send_timeout 120s;
    }

    # 其余 HTTP API 正常反代
    location / {
        proxy_pass http://127.0.0.1:18083;
        proxy_set_header Host $host;
    }
}
```

证书获取：

```bash
# certbot（ACME 正式证书替代自签——消除证书来源指纹）
sudo apt install nginx certbot python3-certbot-nginx
sudo certbot --nginx -d sproxy.example.com
```

## 5. 托管 CDN（Cloudflare）要点

- **WS 支持**：Cloudflare 免费计划即支持 WebSocket，无需额外开通。
- **TLS 模式**：选择 **Full (strict)**——证书须为源站有效证书（可用自签，
  但推荐 ACME 正式证书消除指纹）。
- **回源**：CDN 回源到源站 WS 监听端口；回源地址用 `127.0.0.1` + 反代或直接
  内网 IP + WS 端口。
- **缓存**：WS 路径**不要**开缓存（动态内容）；在 Cache Rules 里把 WS 路径排除。
- **代理设置**：`Proxy status: Proxied`（橙色云朵）→ WS 流量走 CDN。

## 6. 传输策略：质量触发动态切换

服务端/客户端已支持**质量触发动态切换**（roadmap §5.3 P2，mesh connect
`--quality-routing` 及服务端 `QualitySwitchMonitor`）：

- **劣化检测**：周期采样传输重传率，超过阈值判定劣化。
- **自动切换**：劣化连接触发重拨（新传输/新路径），防抖 30s 防抖避免抖动。
- **手动锁定**：`--quality-lock` 手动锁定当前连接，禁自动切换（运维排障用）。
- **可观测**：`sproxy_mesh_switch_total` 指标 + 日志记录切换事件；
  可关闭（`quality_switch.enabled: false`），禁静默降级。

配合 CDN 前置：CDN 故障/劣化时客户端自动重拨（走回源直连或备用传输），
日志与指标证据明确。

## 7. 排障清单

| 症状 | 排查 |
|------|------|
| WS 升级 400 | `upgrade_header` 未同步（sclient `--ws-upgrade-header`） |
| WS 连接立即断开 | Nginx `proxy_read_timeout` < 心跳 90s；`map` 未配 Upgrade 头 |
| 证书指纹仍可识别 | CDN/Nginx 未终结 TLS（源站仍直连 443） |
| 客户端 401/403 | CDN 回源路径与 `ws.path` 不一致；或源站启用了隧道认证 |
| 切换频繁 | `quality_switch.debounce` 调大；确认非 CDN 抖动 |
| 升级头被剥 | 托管 CDN 未开 WS 支持；自建未配 `map $http_upgrade` |

## 8. 验证命令

```bash
# 1. 源站 WS 监听就绪
ss -ltn | grep 18084

# 2. 客户端经 CDN 域名连接（升级头同步）
sclient relay --transport ws --ws-url wss://sproxy.example.com/api/v1/stream \
    --ws-upgrade-header secret-fp --server <mesh-server>

# 3. 隧道往返验证
sclient tunnel https://sproxy.example.com/healthz

# 4. 切换指标
curl -s http://127.0.0.1:18083/metrics | grep mesh_switch
```
