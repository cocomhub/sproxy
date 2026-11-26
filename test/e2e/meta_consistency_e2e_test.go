// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

// 跨存储 meta 一致性验收（nightly/手工）：
//
//	SPROXY_META_E2E_URL=https://127.0.0.1:18084 \
//	SPROXY_META_E2E_AK=... SPROXY_META_E2E_SK=... SPROXY_META_E2E_SKID=... \
//	SPROXY_META_E2E_VOLUMES=main,vault,dav,s3 \
//	go test -tags=e2e -run TestMetaConsistencyRealVolumes -v ./test/e2e
//
// 每个卷执行：普通上传（meta_extra/mtime）→ GetMetaInfo → Stat → meta 更新 →
// 分块上传（meta_extra）→ GetMetaInfo → 清理。未配置 env 自动 skip。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMetaConsistencyRealVolumes(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("SPROXY_META_E2E_URL"), "/")
	ak, sk, skid := os.Getenv("SPROXY_META_E2E_AK"), os.Getenv("SPROXY_META_E2E_SK"), os.Getenv("SPROXY_META_E2E_SKID")
	bearer := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_BEARER"))
	volsRaw := strings.TrimSpace(os.Getenv("SPROXY_META_E2E_VOLUMES"))
	if base == "" || volsRaw == "" || (bearer == "" && (ak == "" || sk == "" || skid == "")) {
		t.Skip("需要 SPROXY_META_E2E_URL/VOLUMES 以及 AK/SK/SKID 或 BEARER")
	}
	vols := strings.Split(volsRaw, ",")
	payload := []byte("meta-consistency-cross-storage")
	local := t.TempDir() + "/meta-e2e.bin"
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(local, old, old); err != nil {
		t.Fatal(err)
	}
	for _, vol := range vols {
		vol = strings.TrimSpace(vol)
		if vol == "" {
			continue
		}
		t.Run(vol, func(t *testing.T) {
			opts := []client.Option{
				client.WithVolume(vol),
				client.WithTimeout(60 * time.Second),
			}
			if bearer != "" {
				opts = append(opts, client.WithBearerToken(bearer))
			} else {
				opts = append(opts, client.WithAccessKey(ak, sk), client.WithAccessKeyID(skid))
			}
			fc := client.NewFileClient(base, opts...)
			ctx := context.Background()
			remote := fmt.Sprintf("meta-e2e-%d.bin", time.Now().UnixNano())
			extra := map[string]any{"x_meta_e2e": vol}
			t.Cleanup(func() {
				_ = fc.Delete(context.Background(), remote, "")
			})

			if _, err := fc.Upload(ctx, local, remote, client.WithMetaExtra(extra)); err != nil {
				t.Fatalf("normal upload: %v", err)
			}
			mi, err := fc.GetFileMetaInfo(ctx, remote)
			if err != nil || mi.Extra["x_meta_e2e"] != vol {
				t.Fatalf("normal meta: extra=%+v err=%v", mi.Extra, err)
			}
			assertMetaMTime(t, "normal", mi.MTime, old)
			if uerr := fc.UpdateFileMeta(ctx, remote, map[string]any{"x_meta_update": "ok"}); uerr != nil {
				t.Fatalf("update meta: %v", uerr)
			}
			mi, err = fc.GetFileMetaInfo(ctx, remote)
			if err != nil || mi.Extra["x_meta_update"] != "ok" {
				t.Fatalf("updated meta: extra=%+v err=%v", mi.Extra, err)
			}
			fi, err := fc.Stat(ctx, remote)
			if err != nil || fi == nil {
				t.Fatalf("stat: %v", err)
			}
			if got := time.Unix(0, fi.ModTime).UTC(); got.Sub(old) > time.Second || old.Sub(got) > time.Second {
				t.Fatalf("stat mtime=%s want %s", got, old)
			}

			chunked := fmt.Sprintf("meta-e2e-chunked-%d.bin", time.Now().UnixNano())
			t.Cleanup(func() { _ = fc.Delete(context.Background(), chunked, "") })
			if _, cerr := fc.ChunkedUpload(ctx, local, chunked, client.WithChunkedMetaExtra(extra)); cerr != nil {
				t.Fatalf("chunked upload: %v", cerr)
			}
			cmi, err := fc.GetFileMetaInfo(ctx, chunked)
			if err != nil || cmi.Extra["x_meta_e2e"] != vol {
				t.Fatalf("chunked meta: extra=%+v err=%v", cmi.Extra, err)
			}
			assertMetaMTime(t, "chunked", cmi.MTime, old)
		})
	}
}

// assertMetaMTime 校验 meta 读取的 mtime 与客户端声明值一致（秒级容差，兼容各后端精度）。
func assertMetaMTime(t *testing.T, label, raw string, want time.Time) {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s meta mtime=%q 无法解析: %v", label, raw, err)
	}
	if delta := got.Sub(want); delta > time.Second || delta < -time.Second {
		t.Fatalf("%s meta mtime=%s want %s", label, got, want)
	}
}
