// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// share_external_volume_test.go —— 分享**外部卷/加密卷**源文件的回归（缺陷修复 2026-10-21）。
//
// 用户实测：`vault`（secretdata over 百度网盘）里的文件创建分享返回 404。根因：分享创建/访问
// 只经 `locateOwnerFile`（**本地卷租户根**）定位源文件 ⇒ 外部卷（加密卷/网盘卷/S3）恒不命中。
//
// 本用例用**真实 secretdata 外部卷**（含加解密）钉住：
//  1. 显式 `volume` 创建分享 → 访问返回**明文**（卷层透明解密），长度也是明文长度；
//  2. 不传 `volume` 时的自动定位也能命中外部卷（本地卷无同名文件时）；
//  3. 多卷同名 rel：显式 `volume` 决定分享源（缺省按「本地卷优先」的既有语义）；
//  4. 源文件被删除后访问 ⇒ 410（fail-closed）。

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// newShareExternalVolumeHandlers 构造带「本地默认卷 + secretdata 外部卷 vault」的 Handlers。
func newShareExternalVolumeHandlers(t *testing.T) (*Handlers, registry.ExternalBackend) {
	t.Helper()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	h.shareStore = NewShareStore(testLogger())

	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	be, err := secretdata.NewBackend(context.Background(),
		volume.Volume{Name: "vault", Type: "secretdata"}, inner,
		secretdata.Options{Secret: []byte("share-vault-key-000"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("secretdata.NewBackend: %v", err)
	}
	vs := registry.NewSet(
		[]volume.Volume{{Name: "default", RootDir: root}, {Name: "vault", Type: "secretdata"}},
		map[string]*storage.Root{"default": h.globalRoot},
		map[string]registry.ExternalBackend{"vault": be},
		nil, "default",
	)
	t.Cleanup(func() { _ = vs.Close() })
	h.volSet = vs
	return h, be
}

// putVaultFile 直接经 secretdata 卷 FS 写文件（键 = <owner>/user/<rel>，与外部卷唯一映射同口径）。
func putVaultFile(t *testing.T, be registry.ExternalBackend, owner, rel string, data []byte) {
	t.Helper()
	key := owner + "/user/" + strings.TrimPrefix(rel, "user/")
	if err := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, key,
		bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("写入 vault 文件 %q: %v", key, err)
	}
}

// createShareReq 调 createShareHandler（owner 经 ctx 注入，与生产认证层同法）。
func createShareReq(t *testing.T, h *Handlers, owner, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/share", strings.NewReader(body))
	req = req.WithContext(withActor(req.Context(), owner))
	rec := httptest.NewRecorder()
	h.createShareHandler(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// accessShareReq 调 accessShareHandler，返回状态码与响应体。
func accessShareReq(t *testing.T, h *Handlers, token string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/s/"+token, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	h.accessShareHandler(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestShare_ExternalEncryptedVolume 外部加密卷分享：显式卷 + 自动定位 + 明文返回。
func TestShare_ExternalEncryptedVolume(t *testing.T) {
	t.Parallel()
	h, be := newShareExternalVolumeHandlers(t)
	plain := []byte("vault-shared-plaintext-payload-0123456789")
	putVaultFile(t, be, "anonymous", "docs/secret.txt", plain)

	// ① 显式 volume 创建分享 → 访问返回明文。
	code, out := createShareReq(t, h, "anonymous",
		`{"filename":"docs/secret.txt","volume":"vault","ttl":"1h"}`)
	if code != http.StatusOK {
		t.Fatalf("显式卷创建分享 = %d %v（外部卷源文件必须可分享）", code, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatalf("分享响应无 token: %v", out)
	}
	sc, got := accessShareReq(t, h, token)
	if sc != http.StatusOK {
		t.Fatalf("访问分享 = %d body=%s", sc, got)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("外部加密卷分享必须返回明文, got %q", got)
	}

	// ② 不传 volume：自动定位（本地默认卷无同名文件 ⇒ 命中外部卷）。
	code2, out2 := createShareReq(t, h, "anonymous", `{"filename":"docs/secret.txt","ttl":"1h"}`)
	if code2 != http.StatusOK {
		t.Fatalf("自动定位创建分享 = %d %v（缺省必须能扫到外部卷）", code2, out2)
	}
	tok2, _ := out2["token"].(string)
	if sc2, got2 := accessShareReq(t, h, tok2); sc2 != http.StatusOK || !bytes.Equal(got2, plain) {
		t.Fatalf("自动定位分享访问 = %d body=%q", sc2, got2)
	}
}

// TestShare_ExternalVolume_HintWinsOverLocal 多卷同名 rel 时显式 volume 决定源（缺省本地优先）。
func TestShare_ExternalVolume_HintWinsOverLocal(t *testing.T) {
	t.Parallel()
	h, be := newShareExternalVolumeHandlers(t)
	localData := []byte("LOCAL-copy")
	remoteData := []byte("VAULT-copy")
	putVaultFile(t, be, "anonymous", "dup.txt", remoteData)
	// 本地默认卷写同名文件（经租户根，用户可见写入口）。
	tnt := h.tenantFor("anonymous")
	if mkErr := tnt.Root().MkdirAll(tnt.UserRoot(), 0o755); mkErr != nil {
		t.Fatalf("建用户桶: %v", mkErr)
	}
	w, werr := writeUserVisible(tnt.Root(), tnt.UserRoot()+"/dup.txt", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("写本地同名文件: %v", werr)
	}
	if _, err := w.Write(localData); err != nil {
		t.Fatalf("写本地同名文件内容: %v", err)
	}
	_ = w.Close()

	// 显式 vault ⇒ 拿到外部卷内容。
	code, out := createShareReq(t, h, "anonymous", `{"filename":"dup.txt","volume":"vault","ttl":"1h"}`)
	if code != http.StatusOK {
		t.Fatalf("显式 vault 分享 = %d %v", code, out)
	}
	tok, _ := out["token"].(string)
	if sc, got := accessShareReq(t, h, tok); sc != http.StatusOK || !bytes.Equal(got, remoteData) {
		t.Fatalf("显式 vault 分享内容 = %d %q, want %q", sc, got, remoteData)
	}

	// 缺省 ⇒ 本地卷优先（既有语义，零回归）。
	code2, out2 := createShareReq(t, h, "anonymous", `{"filename":"dup.txt","ttl":"1h"}`)
	if code2 != http.StatusOK {
		t.Fatalf("缺省分享 = %d %v", code2, out2)
	}
	tok2, _ := out2["token"].(string)
	if sc2, got2 := accessShareReq(t, h, tok2); sc2 != http.StatusOK || !bytes.Equal(got2, localData) {
		t.Fatalf("缺省分享应取本地卷内容, got %d %q", sc2, got2)
	}
}

// TestShare_ExternalVolume_SourceRemoved 源文件删除后访问分享 ⇒ 410（fail-closed）。
func TestShare_ExternalVolume_SourceRemoved(t *testing.T) {
	t.Parallel()
	h, be := newShareExternalVolumeHandlers(t)
	putVaultFile(t, be, "anonymous", "gone.txt", []byte("bye"))
	code, out := createShareReq(t, h, "anonymous", `{"filename":"gone.txt","volume":"vault","ttl":"1h"}`)
	if code != http.StatusOK {
		t.Fatalf("创建分享 = %d %v", code, out)
	}
	tok, _ := out["token"].(string)

	if err := be.FS().Delete(context.Background(), "anonymous/user/gone.txt"); err != nil {
		t.Fatalf("删除 vault 文件: %v", err)
	}
	sc, body := accessShareReq(t, h, tok)
	if sc != http.StatusGone {
		t.Fatalf("源文件删除后访问分享应 410, got %d body=%s", sc, body)
	}
}

// TestAISample_ExternalEncryptedVolume AI 摘要抽样必须支持外部/加密卷（此前只用默认租户根
// ⇒ 「文件不存在」）。缺陷修复 2026-10-21。
func TestAISample_ExternalEncryptedVolume(t *testing.T) {
	t.Parallel()
	h, be := newShareExternalVolumeHandlers(t)
	plain := []byte("vault ai sample text 1234567890")
	putVaultFile(t, be, "anonymous", "ai/doc.txt", plain)

	got, _, err := h.aiSample(context.Background(), "anonymous", "user/ai/doc.txt", "")
	if err != nil {
		t.Fatalf("外部卷 aiSample: %v（缺省自动定位应命中外部卷）", err)
	}
	if !strings.HasPrefix(got, "vault ai sample text") {
		t.Fatalf("抽样内容不符: %q", got)
	}
	// 显式卷提示同口径。
	if _, _, err := h.aiSample(context.Background(), "anonymous", "user/ai/doc.txt", "vault"); err != nil {
		t.Fatalf("显式卷 aiSample: %v", err)
	}
	// 不存在的文件仍报错（fail-closed）。
	if _, _, err := h.aiSample(context.Background(), "anonymous", "user/ai/none.txt", ""); err == nil {
		t.Fatal("不存在的文件不应返回 nil error")
	}
}

// TestArchive_ExternalVolumeFailsLoudly 服务端归档目前只支持本地卷：外部/加密卷文件必须
// **显式失败**（此前静默缺项 ⇒ 「归档成功但少文件」）。缺陷修复 2026-10-21。
func TestArchive_ExternalVolumeFailsLoudly(t *testing.T) {
	t.Parallel()
	h, be := newShareExternalVolumeHandlers(t)
	putVaultFile(t, be, "anonymous", "arch/doc.txt", []byte("payload"))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	req := httptest.NewRequest(http.MethodPost, "/api/archive", nil)
	fileErr, aborted := h.archiveWriteFiles(tw, req, "anonymous", []string{"arch/doc.txt"}, testLogger())
	_ = tw.Close()
	if aborted != nil {
		t.Fatalf("不应因客户端断开中止: %v", aborted)
	}
	if fileErr == nil {
		t.Fatal("外部卷文件不可服务端归档 ⇒ 必须返回错误（静默缺项会让「归档成功」缺文件）")
	}
	if !strings.Contains(fileErr.Error(), "外部卷") {
		t.Fatalf("错误文案应指明原因（外部/加密卷）：%v", fileErr)
	}
}
