// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/ops"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// secrets_api_test.go 覆盖 /api/secrets 端点的 handler 逻辑（创建/列表/导出/删除）。
//
// 寻址模型（用户裁定 2026-10-15）：默认 secrets 卷是 **per-owner** 的——每个 owner 租户根下
// **meta 桶内**的 `meta/secrets/` 目录（`<storage_root>/<owner>/meta/secrets/`），由**请求 owner**
// 解析、首次使用自动派生。故本测试显式注入请求主体（`withActor`）并断言**磁盘落点**按 owner
// 隔离；不再有任何「默认 secrets 卷装配」步骤（这也是被删掉的冗余模型的回归锚点）。

// newSecretsHandlers 装配 secret 端点测试用 Handlers（含真实存储根 ⇒ 租户可用）。
func newSecretsHandlers(t *testing.T, modifyCfg func(*Config)) (*Handlers, string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	if !strings.Contains(cfg.Addr, "127.0.0.1") && !strings.HasPrefix(cfg.Addr, ":") {
		cfg.Addr = "127.0.0.1" + cfg.Addr
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() {
		_ = h.Close()
	})
	return h, tmpDir
}

// ownerReq 构造带操作主体（SproxySig AK）的请求：/api/secrets 的 owner 归属取自请求 ctx
// （ownerFromRequest ⊕ normalizeOwner）。空 owner = 未认证 → anonymous 租户。
func ownerReq(method, target, body, owner string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if owner == "" {
		return req
	}
	return req.WithContext(withActor(req.Context(), owner))
}

// callSecretsHandler 白盒分发到对应 secret handler 方法（绕过 authMiddleware/路由层，
// 聚焦 handler 逻辑；认证面由 TestSecretsRoutesRegistered 验证）。owner 为请求主体。
// method/target 支持：POST /api/secrets、GET /api/secrets、GET /api/secrets/{name}、
// DELETE /api/secrets/{name}。
func callSecretsHandler(h *Handlers, owner, method, target, body string) (int, string) {
	req := ownerReq(method, target, body, owner)
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodPost:
		h.createSecretHandler(rec, req)
	case method == http.MethodGet && strings.TrimPrefix(target, "/api/secrets") == "":
		h.listSecretsHandler(rec, req)
	case method == http.MethodGet:
		req.SetPathValue("name", strings.TrimPrefix(target, "/api/secrets/"))
		h.exportSecretHandler(rec, req)
	case method == http.MethodDelete:
		req.SetPathValue("name", strings.TrimPrefix(target, "/api/secrets/"))
		h.deleteSecretHandler(rec, req)
	default:
		rec.WriteHeader(http.StatusMethodNotAllowed)
	}
	return rec.Code, rec.Body.String()
}

// callJSONHandler 直接调用 http.Handler（含路由），返回状态码与响应体。
func callJSONHandler(h http.Handler, method, target, body string) (int, string) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// secretsOnDisk 返回 owner 默认 secrets 桶内某 secret 的物理路径
// （`<root>/<owner>/meta/secrets/<name>`——meta 桶内，见 pkg/server/secrets_api.go 文件头）。
func secretsOnDisk(root, owner, name string) string {
	return filepath.Join(root, owner, "meta", "secrets", name)
}

// readSecretOnDisk 读 owner 桶内的 secret 文件内容（trim）。不存在/读失败即 Fatal。
func readSecretOnDisk(t *testing.T, root, owner, name string) string {
	t.Helper()
	b, err := os.ReadFile(secretsOnDisk(root, owner, name))
	if err != nil {
		t.Fatalf("读磁盘 secret %s/%s: %v", owner, name, err)
	}
	return strings.TrimSpace(string(b))
}

// TestSecretsRoutesRegistered 验证 /api/secrets 四端点已在主 mux 注册：直接经
// h.Handler()（含路由）请求，非 404 即证明路由存在（loopback 测试模式放行认证，
// 故不断言 401，只断言「路由命中」）。
func TestSecretsRoutesRegistered(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)
	// POST 主 mux 面。
	code, body := callJSONHandler(h.Handler(), http.MethodPost, "/api/secrets", `{"name":"x","mode":"random"}`)
	if code == http.StatusNotFound {
		t.Errorf("主 mux POST /api/secrets 应已注册（404=未挂载），body=%s", body)
	}
	// GET 列表。
	code, _ = callJSONHandler(h.Handler(), http.MethodGet, "/api/secrets", "")
	if code == http.StatusNotFound {
		t.Errorf("主 mux GET /api/secrets 应已注册（404=未挂载）")
	}
}

// decJSON 解析 JSON 响应体到任意结构。
func decJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("解析 JSON 响应失败: %v (body=%s)", err, body)
	}
}

const testSecretHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestSecretsCreateRandom 创建随机 secret：服务端生成 64-hex 并落**请求者 owner** 的桶。
func TestSecretsCreateRandom(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)

	code, body := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"rand1","mode":"random"}`)
	if code != http.StatusOK {
		t.Fatalf("创建随机 secret 状态=%d body=%s", code, body)
	}
	var resp secretCreateResponse
	decJSON(t, body, &resp)
	if resp.Name != "rand1" || resp.Origin != "random" {
		t.Errorf("响应=%+v（应 name=rand1 origin=random）", resp)
	}
	if len(resp.Value) != 64 {
		t.Errorf("随机 secret 返回长度=%d 应 64 hex", len(resp.Value))
	}
	// 落盘断言：<root>/alice/meta/secrets/rand1（owner 的 meta 桶，非全局目录）。
	if got := readSecretOnDisk(t, tmpDir, "alice", "rand1"); got != resp.Value {
		t.Errorf("磁盘值=%q 与返回 %q 不一致", got, resp.Value)
	}
	// Manager 链路可读回（同一 owner 解析出的句柄）。
	mgr, err := h.secretsManagerFor("alice")
	if err != nil || mgr == nil {
		t.Fatalf("secretsManagerFor(alice): mgr=%v err=%v", mgr, err)
	}
	back, rerr := mgr.Read(context.Background(), "rand1")
	if rerr != nil || string(back) != resp.Value {
		t.Errorf("Read 回读 err=%v value=%q", rerr, back)
	}
}

// TestSecretsAnonymousOwner 未认证请求落 anonymous 租户的桶（owner 归一语义不变）。
func TestSecretsAnonymousOwner(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)
	code, body := callSecretsHandler(h, "", http.MethodPost, "/api/secrets", `{"name":"anon1","mode":"random"}`)
	if code != http.StatusOK {
		t.Fatalf("未认证创建状态=%d body=%s", code, body)
	}
	if _, err := os.Stat(secretsOnDisk(tmpDir, "anonymous", "anon1")); err != nil {
		t.Errorf("未认证 secret 应落 <root>/anonymous/meta/secrets/anon1: %v", err)
	}
}

// TestSecretsPerOwnerIsolation 是本重构的**核心不变量**：同名 secret 在不同 owner 下互不可见
// （先前实现是所有 owner 共享 `<storage_root>/secrets/` 单目录，无隔离）。
//
// 断言维度：值不同（各自生成）→ 列表各自独立 → 导出互不串 → 删除互不影响 → 磁盘落点分叉。
func TestSecretsPerOwnerIsolation(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)
	const name = "same-name"

	// 两名 owner 各建同名 secret。
	_, aBody := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"`+name+`","mode":"random"}`)
	var aliceRes secretCreateResponse
	decJSON(t, aBody, &aliceRes)
	_, bBody := callSecretsHandler(h, "bob", http.MethodPost, "/api/secrets", `{"name":"`+name+`","mode":"random"}`)
	var bobRes secretCreateResponse
	decJSON(t, bBody, &bobRes)

	if aliceRes.Value == "" || bobRes.Value == "" || aliceRes.Value == bobRes.Value {
		t.Fatalf("同名 secret 的两个 owner 值应各自独立且非空：alice=%q bob=%q", aliceRes.Value, bobRes.Value)
	}
	// 磁盘：两个不同路径、内容各自等于本 owner 的返回值。
	if got := readSecretOnDisk(t, tmpDir, "alice", name); got != aliceRes.Value {
		t.Errorf("alice 磁盘值=%q want %q", got, aliceRes.Value)
	}
	if got := readSecretOnDisk(t, tmpDir, "bob", name); got != bobRes.Value {
		t.Errorf("bob 磁盘值=%q want %q", got, bobRes.Value)
	}
	// 导出各自独立。
	_, aExp := callSecretsHandler(h, "alice", http.MethodGet, "/api/secrets/"+name, "")
	var aE secretCreateResponse
	decJSON(t, aExp, &aE)
	_, bExp := callSecretsHandler(h, "bob", http.MethodGet, "/api/secrets/"+name, "")
	var bE secretCreateResponse
	decJSON(t, bExp, &bE)
	if aE.Value != aliceRes.Value || bE.Value != bobRes.Value {
		t.Errorf("导出串号：alice=%q bob=%q", aE.Value, bE.Value)
	}
	// bob 删除不影响 alice。
	if code, _ := callSecretsHandler(h, "bob", http.MethodDelete, "/api/secrets/"+name, ""); code != http.StatusOK {
		t.Errorf("bob 删除应 200 得 %d", code)
	}
	if _, err := os.Stat(secretsOnDisk(tmpDir, "alice", name)); err != nil {
		t.Errorf("bob 删除后 alice 的 secret 应仍在: %v", err)
	}
	// 反向：bob 视角已空，alice 视角仍在。
	if code, body := callSecretsHandler(h, "bob", http.MethodGet, "/api/secrets", ""); code != http.StatusOK || strings.Contains(body, name) {
		t.Errorf("bob 列表应不含已删的 %s: code=%d body=%s", name, code, body)
	}
	_, aList := callSecretsHandler(h, "alice", http.MethodGet, "/api/secrets", "")
	var lr secretsListResponse
	decJSON(t, aList, &lr)
	if len(lr.Secrets) != 1 || lr.Secrets[0] != name {
		t.Errorf("alice 列表=%v（应 [%s]）", lr.Secrets, name)
	}
}

// TestSecretsCreateImport 导入路径（客户端本地派生结果上传）+ 非法值/mode fail-closed。
func TestSecretsCreateImport(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)

	// 合法导入：客户端本地派生结果上传。
	code, body := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"imp1","mode":"import","value":"`+testSecretHex+`","origin":"passphrase"}`)
	if code != http.StatusOK {
		t.Fatalf("导入 secret 状态=%d body=%s", code, body)
	}
	var resp secretCreateResponse
	decJSON(t, body, &resp)
	if resp.Name != "imp1" || resp.Origin != "passphrase" || resp.Value != "" {
		t.Errorf("响应=%+v（应 name=imp1 origin=passphrase 无 value）", resp)
	}
	if got := readSecretOnDisk(t, tmpDir, "alice", "imp1"); got != testSecretHex {
		t.Errorf("导入落盘=%q want %q", got, testSecretHex)
	}

	// 非法值 fail-closed：非 hex / 短 / 空。
	for _, bad := range []struct{ name, body string }{
		{"非hex", `{"name":"bad1","mode":"import","value":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`},
		{"短", `{"name":"bad2","mode":"import","value":"abcdef"}`},
		{"空value", `{"name":"bad3","mode":"import","value":""}`},
	} {
		bc, bbody := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", bad.body)
		if bc != http.StatusBadRequest {
			t.Errorf("%s: 应 400 得 %d body=%s", bad.name, bc, bbody)
		}
	}

	// 未知 mode fail-closed。
	uc, _ := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"m1","mode":"bogus"}`)
	if uc != http.StatusBadRequest {
		t.Errorf("未知 mode 应 400 得 %d", uc)
	}
}

// TestSecretsListExportDelete 列表（排序）/导出/删除（不存在 fail-closed）+ 删除后不可读。
func TestSecretsListExportDelete(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)
	const owner = "alice"

	mgr, err := h.secretsManagerFor(owner)
	if err != nil {
		t.Fatalf("secretsManagerFor: %v", err)
	}
	if _, cerr := mgr.Create(context.Background(), "aaa"); cerr != nil {
		t.Fatalf("Create aaa: %v", cerr)
	}
	if _, ierr := mgr.Import(context.Background(), "bbb", []byte(testSecretHex)); ierr != nil {
		t.Fatalf("Import bbb: %v", ierr)
	}

	// 列表（排序）。
	code, body := callSecretsHandler(h, owner, http.MethodGet, "/api/secrets", "")
	if code != http.StatusOK {
		t.Fatalf("列表状态=%d body=%s", code, body)
	}
	var lr secretsListResponse
	decJSON(t, body, &lr)
	if len(lr.Secrets) != 2 || lr.Secrets[0] != "aaa" || lr.Secrets[1] != "bbb" {
		t.Errorf("列表=%v（应 [aaa bbb]）", lr.Secrets)
	}

	// 导出。
	code, body = callSecretsHandler(h, owner, http.MethodGet, "/api/secrets/bbb", "")
	if code != http.StatusOK {
		t.Fatalf("导出状态=%d body=%s", code, body)
	}
	var er secretCreateResponse
	decJSON(t, body, &er)
	if er.Name != "bbb" || er.Value != testSecretHex {
		t.Errorf("导出=%+v（应 value=testSecretHex）", er)
	}

	// 删除不存在 fail-closed（404）。
	code, _ = callSecretsHandler(h, owner, http.MethodDelete, "/api/secrets/nope", "")
	if code != http.StatusNotFound {
		t.Errorf("删除不存在应 404 得 %d", code)
	}

	// 删除存在。
	code, body = callSecretsHandler(h, owner, http.MethodDelete, "/api/secrets/aaa", "")
	if code != http.StatusOK {
		t.Fatalf("删除状态=%d body=%s", code, body)
	}
	if _, rerr := mgr.Read(context.Background(), "aaa"); rerr == nil {
		t.Error("删除后 Read 应失败（secret 已移除）")
	}
	// 列表只剩 bbb。
	lc, lbody := callSecretsHandler(h, owner, http.MethodGet, "/api/secrets", "")
	decJSON(t, lbody, &lr)
	if lc != http.StatusOK {
		t.Errorf("删除后列表状态=%d", lc)
	}
	if len(lr.Secrets) != 1 || lr.Secrets[0] != "bbb" {
		t.Errorf("删除后列表=%v（应 [bbb]）", lr.Secrets)
	}
}

// TestSecretsCreateInvalidName 验证 name 校验（空/含分隔符/点）在 HTTP 层 fail-closed。
func TestSecretsCreateInvalidName(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)

	for _, bad := range []string{"", "a/b", ".", ".."} {
		code, _ := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"`+bad+`","mode":"random"}`)
		if code != http.StatusBadRequest {
			t.Errorf("非法名 %q 应 400 得 %d", bad, code)
		}
	}
}

// TestSecretsHandler_TenantUnavailable 请求者租户不可用（未装配存储根）→ 503 fail-closed
// （绝不回落全局根写 secret）。
func TestSecretsHandler_TenantUnavailable(t *testing.T) {
	t.Parallel()
	var h Handlers // 未装配 tenants/globalRoot
	code, _ := callSecretsHandler(&h, "alice", http.MethodPost, "/api/secrets", `{"name":"x","mode":"random"}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("租户不可用时创建应 503 得 %d", code)
	}
	if _, err := h.secretsManagerFor("alice"); err == nil {
		t.Error("tenantFor 不可用时 secretsManagerFor 应返回错误")
	}
}

// TestSecretsHandler_BucketDerivationFailure 桶派生失败（IO 故障）→ 500（服务端故障，
// **不**误报 503/400）。用「`<root>/<owner>/meta/secrets` 处已存在同名**文件**」逼出 MkdirAll 失败。
func TestSecretsHandler_BucketDerivationFailure(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)
	metaDir := filepath.Join(tmpDir, "alice", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("mkdir owner meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "secrets"), []byte("占位文件"), 0o600); err != nil {
		t.Fatalf("write 占位文件: %v", err)
	}
	code, body := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets",
		`{"name":"ok","mode":"import","value":"`+testSecretHex+`"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("桶派生失败应 500 得 %d body=%s", code, body)
	}
}

// TestSecretsDefaultIgnoresAssembledSecretsVolumes 锁定**去冗余**：`/api/secrets` 恒作用于
// 请求 owner 的桶——**不受** registry 中已装配的 `type: secrets` 卷（含嵌套派生形态）影响。
//
// 历史（本重构删除）：多 secrets 卷并存时按 `extra.default` 标记 / 基础卷优先 / 卷名升序做
// **确定性选取**；per-owner 后默认桶唯一确定，该模型整体消失。本用例即使装配名为
// `default-secrets` 的卷，也断言管理面仍落 owner 桶（不被卷抢占）。
func TestSecretsDefaultIgnoresAssembledSecretsVolumes(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)

	// 装配两个 secrets 卷：① 与默认卷同名的全局卷；② 嵌套派生形态。
	legacyRoot := filepath.Join(tmpDir, "legacy-secrets-root")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatalf("mkdir legacy root: %v", err)
	}
	for _, v := range []volume.Volume{
		{Name: secrets.DefaultVolumeName, Type: "secrets", Extra: map[string]any{"target": "local", "root": tmpDir}},
		{Name: "nested-secrets", Type: "secrets", Extra: map[string]any{"source": "main:alice/user/keys"}},
	} {
		be, err := secrets.NewBackend(t.Context(), v, syncpkg.NewLocalFS(legacyRoot, nil))
		if err != nil {
			t.Fatalf("new backend %s: %v", v.Name, err)
		}
		if err := h.Volumes().AddExternalVolume(v, be); err != nil {
			t.Fatalf("attach %s: %v", v.Name, err)
		}
	}

	// 管理面写入：恒落请求 owner 的桶。
	code, body := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"mine","mode":"random"}`)
	if code != http.StatusOK {
		t.Fatalf("创建状态=%d body=%s", code, body)
	}
	var resp secretCreateResponse
	decJSON(t, body, &resp)
	if got := readSecretOnDisk(t, tmpDir, "alice", "mine"); got != resp.Value {
		t.Errorf("管理面应落 <root>/alice/meta/secrets/mine，磁盘=%q want %q", got, resp.Value)
	}
	// 已装配的 secrets 卷目录内不得出现该 secret（不被抢占）。
	if _, err := os.Stat(filepath.Join(legacyRoot, "mine")); err == nil {
		t.Error("secret 不得落到已装配的 secrets 卷（默认桶恒 per-owner）")
	}
}

// TestSecretsBucket_NotCountedAsUserUsage 锁定**落点归因**（用户裁定 2026-10-15 追加）：per-owner
// 默认 secrets 桶落在 **meta 桶内**（`<owner>/meta/secrets/`）⇒ `stats`/`du` 的桶分类把它归入
// 服务端内部账本（meta），**不计入**该 owner 的用户文件数/用量——避免密钥字节（每 secret 60+ 字节）
// 污染配额水位与文件计数。
//
// 变异锚点：把 `secretsBucketDir` 改回租户根下的裸 `secrets/`（非功能桶名 ⇒ 统计对未知段兜底成
// 「用户文件」）→ 本用例红。
func TestSecretsBucket_NotCountedAsUserUsage(t *testing.T) {
	t.Parallel()
	h, tmpDir := newSecretsHandlers(t, nil)
	code, body := callSecretsHandler(h, "alice", http.MethodPost, "/api/secrets", `{"name":"k","mode":"random"}`)
	if code != http.StatusOK {
		t.Fatalf("创建 secret 状态=%d body=%s", code, body)
	}
	// 先确认确实落盘——否则「统计为 0」只是没写成功的假绿。
	if _, err := os.Stat(secretsOnDisk(tmpDir, "alice", "k")); err != nil {
		t.Fatalf("secret 未落盘（后续统计断言无意义）: %v", err)
	}
	files, size := ops.WalkUploadStats(tmpDir, h.logger)
	if files != 0 || size != 0 {
		t.Errorf("secret 属服务端内部账本（meta 桶），不得计入用户用量：files=%d size=%d", files, size)
	}
}
