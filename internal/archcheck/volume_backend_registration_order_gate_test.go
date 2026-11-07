// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// volume_backend_registration_order_gate_test.go — R45：**外部卷后端类型必须在
// `assembleVolumes` 之前注册**。
//
// 背景（用户实测，2026-10-21）：baidupcs/webdav/sftp/ftp/s3 的类型注册此前在
// `setupSyncVolumeBackends`（`server.RegisterRoutes` **之后**）才执行 ⇒ config 里静态声明
// `type: baidupcs` 等外部卷时，装配期 `registry.NewBackend` 报「未注册后端」并 startup fail
// （只能运行期建卷绕过）。
//
// 判据（贴「决策点」而非「文件里出现过」）：
//  1. `setupServerCore` 内 `registerVolumeBackendTypes()` 调用必须出现在
//     `server.RegisterRoutes(` **之前**（同函数体内的相对顺序）；
//  2. 类型注册不得回退到 sync 阶段：`setupSyncVolumeBackends` 函数体内不允许再出现
//     `webdav.RegisterWebDAVBackend` / `sftp.RegisterSFTPBackend` / `ftp.RegisterFTPBackend` /
//     `s3ext.RegisterS3Backend` / `registerBaidupcsBackend()`（否则又会出现「类型注册晚于装配」）。
//  3. `registerVolumeBackendTypes` 必须覆盖全部**可被 config 静态声明**的外部类型（白名单双查）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVolumeBackendRegistrationOrder 类型注册必须先于卷装配。
func TestVolumeBackendRegistrationOrder(t *testing.T) {
	t.Parallel()
	p := filepath.Join(moduleRoot(t), "cmd", "sproxy", "root.go")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	src := string(b)

	core := funcBody(t, src, "func (rt *runServerRuntime) setupServerCore() error {")
	reg := strings.Index(core, "registerVolumeBackendTypes()")
	routes := strings.Index(core, "server.RegisterRoutes(")
	if reg < 0 {
		t.Fatal("setupServerCore 内未找到 registerVolumeBackendTypes()（外部卷类型注册缺失 ⇒ config 静态声明外部卷会启动失败）")
	}
	if routes < 0 {
		t.Fatal("setupServerCore 内未找到 server.RegisterRoutes(（门禁失效：装配入口已改名？）")
	}
	if reg > routes {
		t.Error("registerVolumeBackendTypes() 出现在 server.RegisterRoutes( **之后** ⇒ assembleVolumes 时后端类型尚未注册" +
			"（config 声明 type: baidupcs/webdav/... 会以「未注册后端」启动失败）")
	}

	sync := funcBody(t, src, "func (rt *runServerRuntime) setupSyncVolumeBackends(")
	for _, banned := range []string{
		"webdav.RegisterWebDAVBackend(",
		"sftp.RegisterSFTPBackend(",
		"ftp.RegisterFTPBackend(",
		"s3ext.RegisterS3Backend(",
		"registerBaidupcsBackend(",
		"registerLinkedBackendSchemas(",
	} {
		if strings.Contains(sync, banned) {
			t.Errorf("setupSyncVolumeBackends 内仍调用 %s —— 类型注册必须留在 registerVolumeBackendTypes"+
				"（sync 阶段晚于 RegisterRoutes ⇒ 静态声明外部卷装配失败）", banned)
		}
	}

	types := funcBody(t, src, "func registerVolumeBackendTypes() {")
	for _, want := range []string{
		"registerBaidupcsBackend()",
		"webdav.RegisterWebDAVBackend()",
		"sftp.RegisterSFTPBackend()",
		"ftp.RegisterFTPBackend()",
		"s3ext.RegisterS3Backend()",
		"registerLinkedBackendSchemas()",
	} {
		if !strings.Contains(types, want) {
			t.Errorf("registerVolumeBackendTypes 缺少 %s —— config 静态声明该类型卷会在装配期「未注册后端」", want)
		}
	}
}

// funcBody 截取以 sig 开头的函数体（到下一个顶层 func 为止）；找不到即失败。
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("未找到函数 %q（门禁失效：函数已改名/移动）", sig)
	}
	rest := src[start+len(sig):]
	if i := strings.Index(rest, "\nfunc "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
