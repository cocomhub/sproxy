# 审查：用户体系（凭据 Ring / SproxySig / TOTP / AK-SK 轮换）

- **批次**：2
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### SproxySig v2 验签（`pkg/server/auth.go:410-490`）
- **凭据定位**：请求头必须携带 `skey-id=<skeyID>`；`ring.GetEntry(ak, skeyID)` **精确取条目**（无试签回退）——条目不存在/非存活即 fail-closed error。
- **签名强度**：`sproxysig.Verify`（crypto/subtle 常量时间比较，`pkg/sproxysig/sproxysig.go:33`）+ nonce 防重放池（`nonceSeen`）。
- **body 防篡改**：`bodyValidator` 流式接收、EOF 与声明哈希比对（`auth.go:465` defer 包装）；JSON 端点 `drainAndVerifyBody` 强制消费剩余 body 触发校验（I-3）。
- **无凭据兜底**：ring 空 → `allow_insecure_loopback` 回环放行 / 其余 401（`auth.go:520-548`）；ring 非空 → 链全失败统一 401。
- **api_keys Bearer 独立链**：`APIKeys.Enabled` 时 Bearer 优先（独立特性，不查 store）。

### 凭据存储（静态加密）
- **AES-256-GCM 信封**：`EncryptWithKey`（`encrypting_crypto.go:31-48`）nonce(12B 随机前置) || GCM ciphertext；**无版本头**（算法升级需整体迁移，注释明示）；**无 AAD**（单文件单 key 下安全，注释记录未来多文件时以 path/owner 作 AAD）。
- **Vault Transit 后端**：KMSStorer 复用 SecureStorer 形态（DEK 信封走 EncryptWithKey）；token 支持 `token_env`/`token_file`。
- **nil 归一**：`normalizeStorer`（`credentialstorer.go`）接口 nil 指针归一（防 nil 接收者 panic）。

### AK/SK 轮换（`credential_rotation.go`）
- **轮换循环**：`rotationPass` 按 rotationConfig 周期扫描；`aliveEntries` 存活判定；`pruneOldKeys` 保留 keepOld 个（旧 key 吊销）。
- **到期提醒**：`notify_before` 提前量（进程内日志告警）。
- **renew 引导**：自 renew（`/api/credentials/{ak}/renew`）允许缺 skey-id（按唯一存活条目定位）；多条目必须显式 skey-id（fail-closed）。

### TOTP 注册/登录（`register_handler.go`）
- 会话 SK + TOTP 双因素；静态加密存储。

### 测试
- `auth_test.go`、`credentials_test.go`、`credential_rotation_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（verifySproxySigFromRing + RingAuthenticator 链 + 静态加密 + 轮换循环）
- `go test -count=1 -timeout 180s -run 'TestAuth|TestCredential|TestSproxySig' ./pkg/server/... ./pkg/sproxysig/... ./pkg/accesskey/...` → **ok**
