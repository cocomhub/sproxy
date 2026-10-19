// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// volume_root_bypass_gate_test.go 是「**卷根直连适配器**」门禁（R26；仓库的 R 号按设计文档
// 局部编号，跨文档不保证唯一，故本文件把口径完整写在头部）。
//
// 背景：`sync.NewLocalFS(<路径>)` 是「任意目录 → 纯 os I/O 的 sync.FS」适配器。一旦它被用在
// **卷根**（或卷根下的目录）上，该面就绕过了 `storage.Root` 提供的四件事：
//  1. **at-rest 透明加解密**（读下发密文 / 写入落明文）；
//  2. **封印**（封装卷占用的源目录仍可被直连读写）；
//  3. **用户可见长度**（`LogicalSize`：长度与配额口径）；
//  4. **卷容量与对账接线**（卷池/账本）。
//
// 历史三个 P0 全部出自这一类，且都靠**逐次评审**才发现（单层测试发现不了——两侧各自有测试，
// 跨层才矛盾）：DAV 面（读密文/写明文 + 绕封印）、备份源（读到密文，见 #773）、同步本地侧
// （pull 落明文 / push 传密文）。设计 `2026-10-11-enclosure-volume` §12 明确要求「任何**新增
// 直连 os I/O 的面**都必须同时接入封印检查」，但此前**没有任何门禁落地**——本门禁把该承诺
// 变成机制。
//
// 口径：全仓非测试源码中 `NewLocalFS(...)` 的**调用**只允许出现在 localFSCallAllowlist
// 登记的文件里，且每条登记必须写明「为什么这里可以不经 Root」。新增调用时先问「能否注入 FS
// 句柄 / 走 `server.NewRootFS`（经 Root）」，而不是加白名单。
//
// 判定用 AST（`go/parser`）而非正则：注释/字符串里的同名文本不误报，`NewLocalFS (` 这类
// 带空格写法也不漏报。
//
// 与 R25（`user_io_gate_test.go`：禁 `root.Open/OpenFile` 读写**用户内容**）互补——R25 管
// 「经 Root 但绕过用户可见入口」，本门禁管「**根本不**经 Root」。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// localFSCallAllowlist 是允许在非测试源码中调用 `sync.NewLocalFS(...)` 的文件（module 相对
// 路径）→ 理由（必须回答「为什么可以不经 storage.Root」）。
var localFSCallAllowlist = map[string]string{
	"pkg/server/audit_archive_sink.go": "**审计冷段归档目录**（`audit.archive_dir`，用户显式配置的独立冷存储目录，可挂载任意卷）：它不在任何受管用户卷根内 ⇒ 无卷/配额语义可依附；归档写入即经该 FS 的 `WriteFile`（与 pkg/sync 的云端转存同源），不是用户内容路径",
	"pkg/volume/enclosure/attach.go":   "封装卷**特权源视图**（必须在封印注入前取得、且不得受封印影响）；源为 at-rest 加密卷时装配期已 ErrSourceEncrypted 显式拒绝（禁双重变换）",
	"cmd/sproxy/secret_register.go":    "**独立目录**回退分支（目标目录不在任何受管卷根内 ⇒ 无卷语义可依附）；落在卷根内的 secrets 目录已改经 Root（`secretsFSFor` → `server.NewRootSecretsFS`，含 0600 回调）",
	"cmd/sproxy/pikpak_secretstore.go": "pikpak 凭据的 **shardseal 加密容器**底层（内容已由 secretdata 加密，非明文密钥材料）；其**主密钥**已改经 Root（`pikpakMasterKeyManager` → `server.NewRootSecretsFS`）",
	"pkg/syncexec/executor.go":         "**回落路径**：仅当装配层未注入 TenantRootFS（测试/未接线）时生效；生产恒注入经 Root 的提供者（cmd/sproxy 装配）",
	"cmd/sclient/sealfile.go":          "**本地独立工具**（sclient 本地加解密，不连服务器、不涉受管卷根）：NewLocalFS 构造**用户指定独立目录**的卷视图（无卷语义可依附）；产物作为 secretdata 卷加载时由服务器 `resolveTargetFS` 经 secretdata 写内核重新装配，非卷根直连旁路",
}

// findLocalFSCalls 解析单个 Go 源文件，返回 `NewLocalFS(...)` 调用表达式的行号（AST 精确
// 判定：不误报注释/字符串里的同名文本，也不漏掉 `NewLocalFS (` 这类带空格写法）。
func findLocalFSCalls(path string) []int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "NewLocalFS" {
			return true
		}
		lines = append(lines, fset.Position(call.Pos()).Line)
		return true
	})
	return lines
}

// TestVolumeRootBypassGate 门禁：非测试源码不得新增未登记的 `NewLocalFS(...)` 调用（见文件头）。
func TestVolumeRootBypassGate(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	scanDirs := []string{"pkg", "cmd", "internal", "tools"}
	var offenders []string
	for _, dir := range scanDirs {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			lines := findLocalFSCalls(path)
			if len(lines) == 0 {
				return nil
			}
			if _, ok := localFSCallAllowlist[rel]; ok {
				return nil // 已登记（登记项自检在下方统一做）
			}
			for _, ln := range lines {
				offenders = append(offenders, rel+":"+strconv.Itoa(ln))
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("未登记的 NewLocalFS 调用（卷根直连适配器，见文件头）：\n  %s\n"+
			"若确需不经 storage.Root，请在 volume_root_bypass_gate_test.go 的 localFSCallAllowlist "+
			"登记并写明理由（优先考虑注入 FS 句柄或走 server.NewRootFS）。",
			strings.Join(offenders, "\n  "))
	}
	// 门禁自检（防「白名单空转」）：每条登记必须仍能解析出调用——文件被删/已改经 Root 后应
	// 同步删除登记项，否则白名单会默默放宽未来判断（门禁静默失效比报红更危险）。
	for rel := range localFSCallAllowlist {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err != nil {
			t.Fatalf("白名单文件不存在（请同步删除登记项）: %s: %v", rel, err)
		}
		if len(findLocalFSCalls(abs)) == 0 {
			t.Fatalf("白名单文件已无 NewLocalFS 调用（请同步删除登记项，避免白名单空转）: %s", rel)
		}
	}
}
