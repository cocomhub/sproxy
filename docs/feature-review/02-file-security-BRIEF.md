# 批次 2 审查：文件服务安全面

> 本批 3 路并发对抗审查：R2.1 多租户+配额 / R2.2 用户体系 / R2.3 版本+分享+归档。
> 基线：master `e428acbe`。产出文件 `02-file-security-*.md`。

## 审查目标功能（roadmap 2.1）

- **R2.1 多租户 + 配额**：租户自包含六桶布局（user/cloud/archive/chunk/version/meta），每租户
  `*os.Root` 防穿越 + `quota.Scope` 双账本（reserve→Commit/Adjust/Release，重启扫描校准）。
- **R2.2 用户体系**：凭据 Ring（SproxySig v2 签名）+ TOTP 注册/登录（session SK）+ AK/SK 轮换 +
  静态加密存储（aesgcm / Vault Transit 后端，token 支持 token_env/token_file）。
- **R2.3 版本管理 + 分享 + 归档**：
  - 版本：versioning.enabled（可选），按卷目录独立计数，restore/删除/GC。
  - 分享：token 分享 + 密码 + 过期 + 一次性/计数，原子持久化（重启恢复）。
  - 归档：POST /api/archive 压缩/解压任务 + sclient archive/archive-dir（zip 打包目录）。

## 关键文件

- R2.1：pkg/server/tenant*.go（租户六桶）、pkg/quota/*.go（Scope 双账本）、pkg/server/quota_*.go、
  pkg/server/volumes.go（卷装配）、os.Root 使用点
- R2.2：pkg/server/auth.go（SproxySig）、pkg/server/credentials.go + credentialstorer.go（凭据 Ring）、
  pkg/accesskey/*.go（签名/加密存储）、pkg/server/register_handler.go（TOTP 注册/登录）、
  pkg/server/credential_rotation.go（AK/SK 轮换）
- R2.3：pkg/server/version.go、pkg/files/version*.go、pkg/server/share.go（分享）、
  pkg/server/archive.go + pkg/server/cloud_archive_handler.go（归档）

## 审查维度与关注点

### 正确性
- quota 双账本一致性（reserve→commit/adjust/release 所有路径，重启扫描校准是否兜底所有泄漏）
- 版本 restore/删除/GC 生命周期；分享 token 原子持久化 + 重启恢复
- 归档任务状态机（压缩/解压、取消、超时）

### 安全性（本批重点）
- 租户隔离：os.Root 是否所有文件操作都走？路径穿越是否在 Root 层面双保险？
- quota 绕过：删除/版本/分享/归档路径是否都正确计入配额？
- 凭据：SproxySig 签名强度、TOTP 会话管理、AK/SK 轮换的旧 key 吊销、静态加密密钥管理
- 分享：token 熵、密码校验、过期/计数语义、暴力破解防护
- 归档：zip 路径穿越（zip slip）、解压路径校验

### 可用性 / 可维护性
- 配额超限错误语义；凭据轮换期间兼容性；分享恢复的幂等性
- 测试覆盖（含 fuzz：路径、token、zip）

## 输出格式

每路审查产出 `02-file-security-<名字>.md`，按 `README.md` 模板。重点给出**证据**（文件:行号 + 代码/测试引用），
发现分级 P0-P3。若无问题面列出「通过项」。
