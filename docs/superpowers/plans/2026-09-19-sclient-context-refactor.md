# sclient 多环境多用户 context 重构实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 sclient 的单份平铺配置重构为 kubectl 式 environments/users/contexts 三件套 + current-context，支持 `--env`/`--user`/`--context` 脚本化与 `sclient context` 命令族切换，register/login/renew 作用于当前环境并自动切用户，mesh/relay/p2p/socks 从 env 取连接面、从 user 取凭据。

**架构：** 新包 `cmd/sclient/internal/contextcfg` 承载配置模型（Environment/User/Context/Config）、加载迁移与解析（flag/env/current 优先级）；`clientfactory.Factory.NewClient` 签名不变、内部改从 context 解析结果构建；新增 `cmd/sclient/context.go` 提供 context/env/user 命令族；trust 凭据命令改为读写当前 context 的 env/user 段。

**技术栈：** Go 1.27，cobra + viper（既有），yaml.v3（既有），XDG 路径（既有）。

**规格：** `docs/superpowers/specs/2026-09-19-sclient-context-refactor-design.md`（本计划的论证依据；执行者两份都读）

## 全局约束

- 解析优先级恒为：`--context` > `--env`+`--user` > `current-context` > 旧 `SCLIENT_ENV` 映射。
- 新配置文件 `~/.config/sproxy/config.yaml`（600），`current-context` 指针写在其中。
- 旧 `sclient.yaml` / `sclient.<env>.yaml` 自动导入为 context，旧文件**不删**（留回滚），打印迁移提示。
- `SCLIENT_CONTEXT` / `SCLIENT_ENV` / `SCLIENT_USER` 环境变量与 flag 同优先级（flag > env > current）。
- 凭据 `access_key_secret` 明文存储 + 文件 600 权限（不引入密钥环）。
- `factory.NewClient(cmd)` 签名不变；`ConfigProvider` 接口保留（内部实现改为 context 解析）。
- 全仓 UTF-8 无 BOM；Go 源文件 SPDX 头；测试纯标准库、仅绑 127.0.0.1；新增顶层 `func TestX` 默认 `t.Parallel()`（无法并发须在 `internal/archcheck/serial_budgets.tsv` 登记理由）。
- 提交信息遵循 Conventional Commits（`feat(scope): ...`），不加署名行。
- `--config <path>` 现指向新 config.yaml；传旧 sclient.yaml 路径时迁移后报错指引。

---

### 任务 1：contextcfg 包 — 配置模型与加载

**文件：**
- 创建：`cmd/sclient/internal/contextcfg/model.go`
- 创建：`cmd/sclient/internal/contextcfg/load.go`
- 测试：`cmd/sclient/internal/contextcfg/contextcfg_test.go`

- [ ] **步骤 1：编写失败的测试（模型 YAML 往返 + 加载默认）**

```go
package contextcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig_LoadAndRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		CurrentContext: "sg",
		Environments: []*Environment{{Name: "sg", ServerURL: "https://hub:18083", HubURL: "wss://hub:18083/ws", NodeID: "home"}},
		Users:         []*User{{Name: "alice", AccessKey: "ak-1", AccessKeySecret: "s1", AccessKeyID: "skey-1", Owner: "alice"}},
		Contexts:      []*Context{{Name: "sg", Environment: "sg", User: "alice"}},
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != "sg" || len(got.Environments) != 1 || got.Environments[0].HubURL != "wss://hub:18083/ws" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Users[0].AccessKeySecret != "s1" {
		t.Errorf("secret round-trip: %q", got.Users[0].AccessKeySecret)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config 文件权限 = %o, want 600", fi.Mode().Perm())
	}
}

func TestLoad_MissingFile_ReturnsDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "nope.yaml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg == nil || len(cfg.Environments) != 0 || cfg.CurrentContext != "" {
		t.Errorf("缺省应为空模型: %+v", cfg)
	}
}

func TestConfig_SetCurrentContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{CurrentContext: "a", Environments: []*Environment{{Name: "a"}}, Contexts: []*Context{{Name: "a"}}}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := SetCurrentContext(path, "a"); err != nil {
		t.Fatalf("SetCurrentContext: %v", err)
	}
	got, _ := Load(path)
	if got.CurrentContext != "a" {
		t.Errorf("current-context 未更新: %q", got.CurrentContext)
	}
}

func TestConfig_ContextValidation(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Environments: []*Environment{{Name: "sg"}},
		Users:        []*User{{Name: "alice"}},
		Contexts:     []*Context{{Name: "c1", Environment: "sg", User: "alice"}, {Name: "c2", Environment: "missing"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "c2") {
		t.Errorf("应报 context c2 引用缺失 environment: %v", err)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd cmd/sclient && GOWORK=off go test ./internal/contextcfg/`
预期：FAIL（package 不存在 / 编译错误）

- [ ] **步骤 3：实现模型与加载**

`model.go`：`Environment`（Name/ServerURL/HubURL/NodeID/CAFile/Insecure/TURN/STUN/VirtualSubnet）、`User`（Name/AccessKey/AccessKeySecret/AccessKeyID/Owner）、`Context`（Name/Environment/User/Volume）、`Config`（APIVersion/Kind/CurrentContext/Environments/Users/Contexts）+ `Validate()`（context 引用必须存在、名字唯一、current 必须存在）。`load.go`：`Load(path)`（缺失返回空模型）、`Save(cfg, path)`（0600 原子写：临时文件 + rename）、`SetCurrentContext(path, name)`（读→改→写）。YAML tag 与设计文档 §3 对齐。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test ./internal/contextcfg/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/internal/contextcfg/
git commit -m "feat(context): contextcfg 包 — environments/users/contexts 模型与加载
（YAML 往返 + 600 权限 + current-context 指针 + 引用校验）"
```

---

### 任务 2：contextcfg 包 — 旧配置迁移 + 解析优先级

**文件：**
- 修改：`cmd/sclient/internal/contextcfg/load.go`
- 创建：`cmd/sclient/internal/contextcfg/resolve.go`
- 测试：`cmd/sclient/internal/contextcfg/contextcfg_test.go`

- [ ] **步骤 1：编写失败的测试（迁移 + 解析优先级）**

```go
func TestMigrateLegacyFlatConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "sclient.yaml")
	os.WriteFile(legacy, []byte("server_url: https://hub:18083\naccess_key: ak-1\naccess_key_secret: s1\naccess_key_id: skey-1\nhub_url: wss://hub:18083/ws\nnode_id: home\n"), 0o600)
	cfg, err := MigrateLegacy(legacy, "")
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(cfg.Environments) != 1 || cfg.Environments[0].Name != "default" {
		t.Errorf("env 名应为 default: %+v", cfg.Environments)
	}
	if cfg.Environments[0].ServerURL != "https://hub:18083" || cfg.Environments[0].HubURL != "wss://hub:18083/ws" {
		t.Errorf("连接面字段映射错: %+v", cfg.Environments[0])
	}
	if cfg.Users[0].AccessKey != "ak-1" || cfg.Users[0].AccessKeySecret != "s1" {
		t.Errorf("凭据面字段映射错: %+v", cfg.Users[0])
	}
	if len(cfg.Contexts) != 1 || cfg.Contexts[0].Name != "default" || cfg.CurrentContext != "default" {
		t.Errorf("context/current 生成错: %+v", cfg)
	}
}

func TestMigrateLegacyWithEnvName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "sclient.prod.yaml")
	os.WriteFile(legacy, []byte("server_url: https://prod:18083\n"), 0o600)
	cfg, err := MigrateLegacy(legacy, "prod")
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if cfg.Environments[0].Name != "prod" {
		t.Errorf("SCLIENT_ENV=prod 映射 env 名 prod: %+v", cfg.Environments)
	}
}

func TestResolve_Priority(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		CurrentContext: "cur",
		Environments:   []*Environment{{Name: "a"}, {Name: "b"}},
		Users:          []*User{{Name: "u1"}, {Name: "u2"}},
		Contexts: []*Context{
			{Name: "cur", Environment: "a", User: "u1"},
			{Name: "sel", Environment: "b", User: "u2"},
		},
	}
	// 1) --context 最高。
	r, err := Resolve(cfg, ResolveArgs{Context: "sel", Environment: "a", User: "u2"})
	if err != nil || r.Environment != "b" || r.User != "u2" {
		t.Errorf("--context 应覆盖 env/user: %+v, %v", r, err)
	}
	// 2) --env+--user 覆盖 current。
	r2, _ := Resolve(cfg, ResolveArgs{Environment: "b", User: "u2"})
	if r2.Environment != "b" || r2.User != "u2" {
		t.Errorf("--env/--user 应覆盖 current: %+v", r2)
	}
	// 3) 无 flag → current。
	r3, _ := Resolve(cfg, ResolveArgs{})
	if r3.Environment != "a" || r3.User != "u1" {
		t.Errorf("默认应走 current: %+v", r3)
	}
	// 4) current 缺失 → 报错指引 context use。
	cfg.CurrentContext = ""
	if _, err := Resolve(cfg, ResolveArgs{}); err == nil || !strings.Contains(err.Error(), "context use") {
		t.Errorf("无 current 且无 flag 应报错指引: %v", err)
	}
}

func TestResolve_SingleOverride(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		CurrentContext: "cur",
		Environments:   []*Environment{{Name: "a"}, {Name: "b"}},
		Users:          []*User{{Name: "u1"}},
		Contexts:       []*Context{{Name: "cur", Environment: "a", User: "u1"}},
	}
	// 只覆盖 user → env 保持 current 的。
	r, _ := Resolve(cfg, ResolveArgs{User: "u1"})
	if r.Environment != "a" {
		t.Errorf("仅 --user 时 env 应保持 current: %+v", r)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd cmd/sclient && GOWORK=off go test ./internal/contextcfg/`
预期：FAIL（MigrateLegacy / Resolve 未定义）

- [ ] **步骤 3：实现迁移与解析**

`load.go` 加 `MigrateLegacy(legacyPath, envName)`：读旧平铺（viper 或 yaml 直接解到 `client.Config`）→ 映射连接面字段到 `Environments[0]`（名 = envName 或 "default"）、凭据面到 `Users[0]` → 生成 context + current。`resolve.go` 加 `Resolve(cfg, ResolveArgs{Context, Environment, User}) (*Resolved, error)`：`Resolved{Environment *Environment, User *User, Volume string, ContextName string}`；按优先级取 environment/user，两者都解析后才有效；引用缺失报错；`--context` 整体覆盖，`--env`/`--user` 单项覆盖 current 的对应项。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test ./internal/contextcfg/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/internal/contextcfg/
git commit -m "feat(context): 旧平铺配置自动迁移 + 解析优先级（context > env/user > current）"
```

---

### 任务 3：root.go 接线 — --env/--user/--context 全局 flag + 解析注入

**文件：**
- 修改：`cmd/sclient/root.go`
- 修改：`cmd/sclient/internal/clientfactory/factory.go`
- 测试：`cmd/sclient/root_test.go`

- [ ] **步骤 1：编写失败的测试（flag 注册 + factory 从 context 取 server/凭据）**

```go
func TestNewRootCmd_ContextFlagsRegistered(t *testing.T) {
	cmd := NewRootCmd()
	f := cmd.PersistentFlags().Lookup("context")
	if f == nil {
		t.Fatal("--context flag 未注册")
	}
	if cmd.PersistentFlags().Lookup("env") == nil || cmd.PersistentFlags().Lookup("user") == nil {
		t.Fatal("--env/--user flag 未注册")
	}
}
```

（factory 集成：mock 配置含两 context，`--context` 选中后 NewClient 的 ServerURL 与凭据来自对应 env/user；在 `cmd/sclient/internal/clientfactory/factory_test.go` 补）

```go
func TestFactory_NewClient_ContextResolved(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{
		CurrentContext: "a",
		Environments:   []*contextcfg.Environment{{Name: "a", ServerURL: "https://a:18083"}, {Name: "b", ServerURL: "https://b:18083"}},
		Users:          []*contextcfg.User{{Name: "ua", AccessKey: "ak-a", AccessKeySecret: "sa", AccessKeyID: "skey-a"}},
		Contexts:       []*contextcfg.Context{{Name: "a", Environment: "a", User: "ua"}},
	}
	if err := contextcfg.Save(cfg, cfgPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 构造 factory（config 路径指向新 config.yaml）+ --context b → server 应为 b、凭据来自 b 的 user
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd cmd/sclient && GOWORK=off go test . ./internal/clientfactory/`
预期：FAIL（--context flag 未定义 / factory 未消费 context）

- [ ] **步骤 3：实现 root.go 接线**

root.go：注册 `--context` / `--env` / `--user` 三个 PersistentFlags；`PersistentPreRunE` 里解析 contextcfg：读取 `SCLIENT_CONTEXT`/`SCLIENT_ENV`/`SCLIENT_USER` 环境变量为默认值（flag 为空时）；调 `contextcfg.Resolve` 得到 `*Resolved`，存到包级变量（供 factory/cfgSvc 用）；config.yaml 缺失时先尝试 `MigrateLegacy` 旧文件自动导入。factory.go：`NewClient` 里从 Resolved 构建 opts（serverURL = Resolved.Environment.ServerURL，凭据 = Resolved.User.*，hub/node_id/TURN/STUN 从 env 回落，flag 仍优先）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test . ./internal/clientfactory/`
预期：PASS（既有测试若引用旧平铺加载需同步修）

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/root.go cmd/sclient/root_test.go cmd/sclient/internal/clientfactory/
git commit -m "feat(context): root 全局 --env/--user/--context flag + factory 从 context 解析构建"
```

---

### 任务 4：sclient context 命令族

**文件：**
- 创建：`cmd/sclient/context.go`
- 测试：`cmd/sclient/context_test.go`

- [ ] **步骤 1：编写失败的测试（context list/use/get/set/delete/rename + env/user）**

```go
func TestContextCmd_ListAndUse(t *testing.T) {
	t.Setenv("SCLIENT_CONTEXT", "")
	t.Setenv("SCLIENT_ENV", "")
	t.Setenv("SCLIENT_USER", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{
		CurrentContext: "a",
		Environments:   []*contextcfg.Environment{{Name: "a", ServerURL: "https://a"}},
		Users:          []*contextcfg.User{{Name: "u1"}},
		Contexts:       []*contextcfg.Context{{Name: "a", Environment: "a", User: "u1"}, {Name: "b", Environment: "a", User: "u1"}},
	}
	if err := contextcfg.Save(cfg, cfgPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var out strings.Builder
	cmd := newContextCommand(&cfgPath)
	cmd.SetArgs([]string{"list"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("context list: %v", err)
	}
	if !strings.Contains(out.String(), "a") || !strings.Contains(out.String(), "b") {
		t.Errorf("list 应含全部 context: %s", out.String())
	}
	// use b
	var out2 strings.Builder
	cmd2 := newContextCommand(&cfgPath)
	cmd2.SetArgs([]string{"use", "b"})
	cmd2.SetOut(&out2)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("context use: %v", err)
	}
	got, _ := contextcfg.Load(cfgPath)
	if got.CurrentContext != "b" {
		t.Errorf("use 后 current 应为 b, got %q", got.CurrentContext)
	}
}

func TestContextCmd_DeleteCurrent_Fails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{CurrentContext: "a", Environments: []*contextcfg.Environment{{Name: "a"}}, Users: []*contextcfg.User{{Name: "u"}}, Contexts: []*contextcfg.Context{{Name: "a", Environment: "a", User: "u"}}}
	_ = contextcfg.Save(cfg, cfgPath)
	var out strings.Builder
	cmd := newContextCommand(&cfgPath)
	cmd.SetArgs([]string{"delete", "a"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatal("删除当前 context 应失败")
	}
}

func TestEnvCmd_ListAndUse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{CurrentContext: "a", Environments: []*contextcfg.Environment{{Name: "a"}, {Name: "b"}}, Users: []*contextcfg.User{{Name: "u"}}, Contexts: []*contextcfg.Context{{Name: "a", Environment: "a", User: "u"}}}
	_ = contextcfg.Save(cfg, cfgPath)
	var out strings.Builder
	cmd := newEnvCommand(&cfgPath)
	cmd.SetArgs([]string{"use", "b"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env use: %v", err)
	}
	got, _ := contextcfg.Load(cfgPath)
	if got.Contexts[0].Environment != "b" {
		t.Errorf("env use 应更新当前 context 的 environment: %+v", got.Contexts[0])
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（newContextCommand / newEnvCommand 未定义）

- [ ] **步骤 3：实现 context 命令族**

`context.go`：`newCmdContext(cfgPath *string)` 含子命令 list（标 `*` 当前）/use/get/set（`--env`/`--user`/`--volume`）/delete（current 拒绝）/rename；`newCmdEnv(cfgPath *string)`（list/use：更新当前 context 的 environment）；`newCmdUser(cfgPath *string)`（list/use：更新当前 context 的 user）。全部读写 config.yaml（`contextcfg.Load`/`Save`/`SetCurrentContext`）。root.go 注册三个命令族。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test -run 'TestContextCmd|TestEnvCmd|TestUserCmd' .`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/context.go cmd/sclient/context_test.go cmd/sclient/root.go
git commit -m "feat(context): sclient context/env/user 命令族 — list/use/get/set/delete/rename 切换上下文"
```

---

### 任务 5：trust register/login/renew 作用于当前 context

**文件：**
- 修改：`cmd/sclient/trust_register.go`
- 修改：`cmd/sclient/trust_login.go`
- 修改：`cmd/sclient/trust.go`
- 测试：`cmd/sclient/trust_register_test.go`（新建）、`cmd/sclient/trust_login_test.go`

- [ ] **步骤 1：编写失败的测试（register 用 context 的 env + 自动切 user）**

```go
// trustRegisterEnv 扩展 trustLoginEnv：config.yaml（contextcfg）驱动而非平铺 sclient.yaml。
// mock 端点同 trustLoginEnv（register/nonce/login）。
func TestTrustRegister_ContextEnv_CreatesUserAndSwitches(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{
		CurrentContext: "sg",
		Environments:   []*contextcfg.Environment{{Name: "sg", ServerURL: srv.URL}},
		Contexts:       []*contextcfg.Context{{Name: "sg", Environment: "sg"}},
	}
	_ = contextcfg.Save(cfg, cfgPath)
	// register alice → 应调用 /api/credentials/register（srv），成功后：
	//   - Users 增加 alice（凭据回填 ak/skey/secret）
	//   - Contexts[0].User == "alice"（自动切换）
	//   - 输出提示用 trust login alice
}

func TestTrustLogin_ContextUser_UsesCurrentUser(t *testing.T) {
	// context 的 user = alice（已含凭据）→ login 用当前 user 的 ak 登录，回填到该 user 段
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（register/login 仍读平铺 sclient.yaml / 不写 context 模型）

- [ ] **步骤 3：实现**

trust_register.go：`runTrustRegister` 改为——取当前 context 的 env（ServerURL）构造 noAuth 客户端 → RegisterTOTP → 成功后 `Users` 增加新 user（名 = owner，凭据回填 ak/空 secret/id）→ `Contexts[0].User = owner`（自动切换）→ Save config.yaml。trust_login.go：取 context 的 env + 当前 user（或位置参数用户名/--ak）→ 登录 → 回填凭据到该 user 段 → Save。trust.go renew 同理回写当前 user 段。`loadTrustLoginConfig` 改为从 context 解析（serverURL 来自 env，凭据来自 user）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test -run 'TestTrustRegister|TestTrustLogin' .`
预期：PASS（既有 trustLoginEnv 测试需适配：cfg 改 contextcfg 驱动或补兼容层）

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/trust_register.go cmd/sclient/trust_login.go cmd/sclient/trust.go cmd/sclient/trust_register_test.go cmd/sclient/trust_login_test.go
git commit -m "feat(context): trust register/login/renew 作用于当前 context（env 注册 + 自动切用户 + 凭据回填）"
```

---

### 任务 6：mesh/relay/p2p/socks 从 env 取连接面

**文件：**
- 修改：`cmd/sclient/internal/clientfactory/factory.go`
- 修改：`cmd/sclient/mesh.go`、`cmd/sclient/relay.go`、`cmd/sclient/p2p.go`、`cmd/sclient/socks.go`
- 测试：`cmd/sclient/internal/clientfactory/factory_test.go`

- [ ] **步骤 1：编写失败的测试（env 的 hub/node_id/TURN 回落）**

```go
// factory 集成：context 的 env 含 HubURL/NodeID/TURN → NewClient 后
// mesh connect 未显式 --hub 时用 env 的 hub；socks 未显式 --turn 时用 env 的 TURN。
func TestFactory_NewClient_EnvMeshDefaults(t *testing.T) {
	// config.yaml：env sg 含 hub_url=wss://hub:18083/ws node_id=home turn=[{uri:...}]
	// NewClient(cmd) → 客户端 mesh 回落字段 = env 的 hub/node_id/turn
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（env 的 mesh 面字段未被 factory 消费）

- [ ] **步骤 3：实现**

factory.go `NewClient`：`WithMeshHubURL`/`WithNodeID` 从 Resolved.Environment 回落（flag 优先，无 flag 时用 env 值）；新增 `WithTURNServers`/`WithSTUNServers`/`WithVirtualSubnet` option（如无现成 option 则加）。mesh.go/relay.go/p2p.go/socks.go：各命令在 `--hub`/`--node-id`/`--turn*`/`--stun`/`--virtual-subnet` 未显式指定时从 env 回落（替代现在从平铺 cfg.HubURL/cfg.NodeID 回落）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test ./internal/clientfactory/ .`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/internal/clientfactory/factory.go cmd/sclient/mesh.go cmd/sclient/relay.go cmd/sclient/p2p.go cmd/sclient/socks.go
git commit -m "feat(context): mesh/relay/p2p/socks 连接面从 context 的 env 回落（hub/node_id/TURN/STUN）"
```

---

### 任务 7：config show/set 适配 + 文档 + 门禁

**文件：**
- 修改：`cmd/sclient/config.go`
- 修改：`docs/config.md`、`docs/cli.md`
- 修改：`internal/archcheck/serial_budgets.tsv`（如新增串行测试）

- [ ] **步骤 1：编写失败的测试（config show 显示当前 context 解析视图）**

```go
func TestConfigCmd_ShowResolvedContext(t *testing.T) {
	// config.yaml：context a（env a + user u1）→ config show 输出 server_url=https://a、access_key=ak-1
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（config show 仍读平铺）

- [ ] **步骤 3：实现**

config.go `NewCmdConfig`：`show` 显示 `Resolved` 解析后的扁平视图（server_url/access_key 等从 env/user 合成）；`set <key> <value>` 写入对应段（server_url→env、access_key→user）。docs/config.md 客户端配置章节重写为 context 模型（environments/users/contexts 结构 + 迁移说明 + 命令示例）；docs/cli.md 补 context 命令族。serial_budgets.tsv 登记新增串行测试（如适用）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd cmd/sclient && GOWORK=off go test . ./internal/... ./internal/archcheck/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/config.go docs/config.md docs/cli.md internal/archcheck/serial_budgets.tsv
git commit -m "feat(context): config show/set 适配当前 context 解析视图 + 文档与门禁登记"
```

---

### 任务 8：全量验证 + 冒烟

**文件：**
- 修改：`internal/archcheck/serial_budgets.tsv`（如需）
- 测试：`cmd/sclient/context_smoke_test.go`（可选冒烟）

- [ ] **步骤 1：全量测试**

运行：
```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/sclient-context
export PATH="$PATH:$(go env GOPATH)/bin"
go test -count=1 -race ./pkg/... ./cmd/... ./internal/archcheck/ 2>&1 | grep -E "^(--- FAIL|ok |FAIL)"
```
预期：全 ok；若有失败逐包修复（重点 cmd/sclient 既有测试适配 context 模型）。

- [ ] **步骤 2：门禁**

运行：`make test && make lint && make lint-all && make deadcode-check && make web-test`
预期：全绿；serial_budgets 若超基线登记新文件。

- [ ] **步骤 3：冒烟（本地手动，fake 服务端）**

```bash
# 1) 迁移：删除测试 HOME 的 config.yaml，放旧 sclient.yaml → sclient context list 应显示导入的 default
# 2) 切换：sclient context use sg → context get 显示 env+user 合并
# 3) register：sclient trust register alice → 输出 ak/base32；config.yaml 的 Users 增 alice、context user 切到 alice
# 4) 脚本化：sclient --env sg --user alice list（mock server）→ 走 sg 的 server
```

- [ ] **步骤 4：Commit（如有冒烟测试）**

```bash
git add cmd/sclient/context_smoke_test.go internal/archcheck/serial_budgets.tsv
git commit -m "test(context): 冒烟测试 — 迁移/切换/register 自动切用户/脚本化 flag"
```

- [ ] **步骤 5：收尾检查**

```bash
gofmt -l cmd/ pkg/ && goimports -l cmd/ pkg/
git status --porcelain   # 只含本任务文件
```
推送 `feat/sclient-context` → `gh pr create` → 等 CI 15 项全绿 → `gh pr merge --squash --delete-branch`（commit_title 保留 `(#PR号)`）→ `git pr-clean` + worktree remove + 主工作区同步。
