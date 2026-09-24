# 设计：IP 白名单 / 信任代理（auth.allow_ips + auth.trusted_proxy）

## 背景与目标

现状（源码事实）：`pkg/server/ratelimit.go` 的 `RateLimiter.Middleware` 与 nonce/login
（`register_handler.go`）用 `normalizeRemoteIP(r.RemoteAddr)`（SplitHostPort 去端口）作
per-IP 键；`auth.go` 的 `authMiddleware` 是认证入口（api_keys 链前分支 → Authenticator 链 →
ring 空兜底），**无任何来源 IP 门**；`config.go` 只有 `RateLimitConfig`（无白名单），
`Config` 无 auth 段。

目标：
- `auth.allow_ips`（CIDR 列表）——认证**前**的 IP 门：不在白名单 → 403；
- `auth.trusted_proxy`（反向代理 IP 列表）——仅信任代理来源的 `X-Forwarded-For`，
  解析出真实客户端 IP 供 IP 门与 per-IP 限流共用；
- 零回归：allow_ips/trusted_proxy 均空 = 特性不启用，行为与现状完全一致。

## 组件与接口

1. `pkg/server/config.go` — 新增配置模型：
   ```go
   type AuthConfig struct {
       AllowIPs      []string `yaml:"allow_ips" mapstructure:"allow_ips"`           // CIDR 列表；空 = 不启用（零回归）
       TrustedProxies []string `yaml:"trusted_proxies" mapstructure:"trusted_proxies"` // 信任的代理 IP/CIDR；空 = 不解析 XFF
   }
   ```
   `Config` 新增字段 `Auth AuthConfig`（`yaml:"auth"`）。
2. `pkg/server/config_validate.go` — 启动校验（fail-closed）：AllowIPs/TrustedProxies
   逐条 `net.ParseCIDR`，非法 → 响亮拒绝启动；CIDR 与纯 IP 混写归一（纯 IP 按 /32、/128
   处理，或要求统一 CIDR——设计取「两种都收，ParseCIDR 优先，失败再 ParseIP→/32」）。
3. `pkg/server/auth.go`（或新 `pkg/server/ipgate.go`）— 核心逻辑：
   - `resolveClientIP(r, trusted []*net.IPNet) string`：RemoteAddr host ∈ TrustedProxies 时
     从 XFF 链**右向左**取第一个非信任代理条目（标准语义）；无/畸形/全信任 → 回退
     RemoteAddr。**未配置 trusted_proxy 时一律忽略 XFF**（防伪造）。
   - `h.ipGate(next http.Handler) http.Handler`：cfg.Auth.AllowIPs 空 → 直通（零回归）；
     非空 → resolveClientIP 不在任何网段 → Warn + 403（"forbidden: ip not allowed"）。
4. `pkg/server/routes.go` — 挂点：
   - `authMiddleware` 内部最前插入 IP 门（覆盖全部认证保护路由：文件组、cloud/sync/
     audit/config/hub/凭据管理/tunnel 等）；
   - 公开凭据端点 register/nonce/login 分别包 `h.ipGate`（白名单部署要求所有入口一致）；
   - **不门** /healthz、/version、/metrics、/ui 静态（探活与静态资源须保持可达，文档明示）；
   - 隧道内层 localMux 不重复门（外层已门，内层是同一请求链）。
5. `pkg/server/ratelimit.go` — per-IP 桶键升级：`Middleware` 改用 `resolveClientIP`
   （装配 trusted_proxy 后限流按真实客户端 IP 计量；未配置时与 normalizeRemoteIP 等价，
   零回归）。nonce 池 IP 绑定（`register_handler.go`）同步用同一解析。

## 数据流

1. 请求 → `h.ipGate`（authMiddleware 首部 / 公开凭据端点包装）：解析真实 IP → 白名单
   判定（未启用直接放行）。
2. 放行后：api_keys 链前分支 / Authenticator 链认证（现状不变）→ handler。
3. 限流：RateLimiter.Middleware 用同一真实 IP 作 per-IP 桶键 → 全局窗口兜底（不变）。
4. nonce 签发绑定解析后的 IP（登录防转发语义在代理后仍按真实客户端生效）。

## 错误处理

- 白名单未命中 → 403（统一文案 "forbidden"），Warn 日志含 path/ip；不落审计（非业务事件，
  防日志洪泛；如需要可在片 2 加审计）。
- XFF 畸形（非 IP、超长链 >64 段）→ 忽略整条 XFF 回退 RemoteAddr（fail-closed，不 panic）。
- 配置非法（坏 CIDR）→ 启动校验拒绝（fail-closed，不静默忽略）。
- allow_ips 配置但 trusted_proxy 未配置：XFF 被忽略，白名单按直连 IP 判定（文档提示：
  反向代理部署必须同时配置 trusted_proxy，否则白名单对代理来源恒 403）。

## 测试 + 变异点

- 测试：
  1. 白名单命中（127.0.0.1 在 allow_ips）→ 200；
  2. 未命中（10.x 不在）→ 403；
  3. trusted_proxy + XFF：代理来源 XFF=真实 IP → resolve 正确；per-IP 桶按真实 IP 计量
     （两个不同 XFF IP 各自独立配额）；
  4. 非信任来源伪造 XFF → 忽略，按 RemoteAddr 判定；
  5. allow_ips 空 → 全放行（零回归断言）；
  6. 坏 CIDR 配置 → 启动校验失败。
- 变异点（改后必红）：① 删 allow_ips 判定 → 测试 2 红；② 不解析 XFF → 测试 3 红；
  ③ 无条件信任 XFF（未配 trusted_proxy 也解析）→ 测试 4 红；④ 白名单误用
  ParseIP（不支持 CIDR）→ 测试 1 红。

## 片划分

- 片 1：AuthConfig + 校验 + resolveClientIP + ipGate + authMiddleware/公开端点挂点 +
  上表测试与变异验证。
- 片 2：ratelimit 桶键与 nonce 绑定切换到 resolveClientIP + 全量回归 + 文档
  （docs/config.md、docs/cli.md 相关段 + R15 门禁同步）。

## 风险与零回归保证

- 零回归面：两配置均空时 ipGate 直通、resolveClientIP 与 normalizeRemoteIP 等价、
  ratelimit 键不变、healthz/version/metrics/ui 不动。
- 风险 1：代理后 per-IP 桶语义变化（多用户共享代理 IP → 桶合并）——仅在显式配置
  trusted_proxy 时发生，属特性启用后的预期行为，文档注明。
- 风险 2：白名单遗漏运维网段导致自锁——允许 loopback 恒放行（运维本机不因误配锁死），
  文档明示；ipGate 对回环来源且 allow_ips 非空时先判白名单、回环不在白名单仍 403 的
  取舍：**设计取回环也须在白名单**（与既有 allow_insecure_loopback 语义分离，避免双通道
  绕行），但启动校验打印「当前 allow_ips 不含 127.0.0.1/::1」告警。
- 风险 3：XFF 链长度 DoS——链解析有界（最多取首个非信任项，天然 O(1)）。
