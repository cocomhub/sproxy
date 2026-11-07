// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// enclosure_tenant_seal_test.go 锁定**运行期封装卷增删对已缓存租户根的封印联动**（F2/阶段 2 复审修复）：
// 租户子根句柄由 TenantCache 缓存（不再经 OpenTenant），故运行期 `SetSeals` 必须对它立即生效——
// 否则 owner 限定声明（足迹 = 该 owner 的可寻址数据区）会被已缓存租户根绕过：客户端经
// `<user>/<sub>/…` 直写封装区间（明文进密文区 / 写保护失效）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	serverenclosure "github.com/cocomhub/sproxy/pkg/server/enclosure"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestEnclosureRuntimeAttach_SealsCachedTenantRoot 运行期新建 owner 限定封装卷后，**先于建卷
// 已缓存**的租户根必须立即被封印拦住（写恒拒）；删除该封装卷后封印同步解除。
//
// 变异（把租户子根封印改回「OpenTenant 时快照」）⇒ 第 3 步写成功、第 5 步仍被拒 ⇒ 本测红。
func TestEnclosureRuntimeAttach_SealsCachedTenantRoot(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0)
	mainDir := h.volSet.Root("main").AbsPath()

	// 1) 先取（并缓存）alice 的默认卷租户根，写一个**封印区间之外**的文件（证明租户根可用）。
	tnt := h.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("alice 租户根不可用")
	}
	writeThrough := func(rel string) error {
		if derr := tnt.Root().MkdirAll(filepath.ToSlash(filepath.Dir(rel)), 0o755); derr != nil {
			return derr
		}
		f, werr := tnt.Root().OpenFile(filepath.ToSlash(rel), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if werr != nil {
			return werr
		}
		_, _ = f.Write([]byte("x"))
		return f.Close()
	}
	if werr := writeThrough("user/other/keep.bin"); werr != nil {
		t.Fatalf("未封印时写应放行: %v", werr)
	}

	// 2) 运行期新建 owner 限定封装卷：target=main/alice/user/docs（足迹 = alice 的可寻址区域）。
	tv, nested := serverenclosure.TranslateNestedVolume(volume.Volume{
		Name: "vault-docs", Type: nestedSecretType,
		Extra: map[string]any{"target": "main/alice/user/docs"},
	})
	if !nested {
		t.Fatal("应为嵌套封装声明")
	}
	if aerr := serverenclosure.AttachEnclosureVolume(context.Background(), h.volSet, testLogger(), mainDir, tv); aerr != nil {
		t.Fatalf("运行期装配失败: %v", aerr)
	}

	// 3) 同一（已缓存）租户根：写入封印区间必须 ErrSealed（模型：全链路写面共用 Root 封印）。
	if werr := writeThrough("user/docs/new.bin"); !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("运行期新增封印必须对已缓存租户根生效（写应 ErrSealed），got %v", werr)
	}
	// 4) 经 HTTP 上传同一路径同样被拦（写面唯一底座 = storage.Root）。
	mux := actorFileWriteMux(h, "alice")
	if st, body := writeMuxUpload(t, mux, "docs/up.bin", "", []byte("payload")); st == 200 {
		t.Fatalf("封印区间上传应被拒, got 200 (body: %s)", body)
	}

	// 5) 删除封装卷（生产时序：摘除运行时卷 → 拆除布线）→ 封印回收，写恢复放行。
	if rerr := h.volSet.RemoveExternalVolume("vault-docs"); rerr != nil {
		t.Fatalf("RemoveExternalVolume: %v", rerr)
	}
	serverenclosure.DetachEnclosureVolume(h.volSet, testLogger(), tv)
	if werr := writeThrough("user/docs/after.bin"); werr != nil {
		t.Fatalf("封装卷删除后租户根写应恢复放行: %v", werr)
	}
}
