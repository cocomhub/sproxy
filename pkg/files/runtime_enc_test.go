// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// spyMetaPolicy 记录 VerifyDownload 是否被调用。
type spyMetaPolicy struct{ called bool }

func (p *spyMetaPolicy) Enabled() bool { return true }

func (p *spyMetaPolicy) WriteMeta(context.Context, string, *storage.Root, string) error {
	return nil
}

func (p *spyMetaPolicy) VerifyDownload(_ context.Context, _ *storage.Root, _ string, r SeekReadCloser) (SeekReadCloser, error) {
	p.called = true
	return r, nil
}

// TestRuntime_VerifyDownload_SkipsEncryptedRoot P0 回归：加密卷的 sidecar 由密文计算，
// 明文解密流不可能匹配 → runtime.verifyDownload 必须直接跳过（与 chunked_download 同口径）。
func TestRuntime_VerifyDownload_SkipsEncryptedRoot(t *testing.T) {
	t.Parallel()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if serr := root.SetEncryption(make([]byte, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	w, err := root.OpenFileEncrypted("f.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFileEncrypted: %v", err)
	}
	if _, cerr := io.Copy(w, strings.NewReader("hello world")); cerr != nil {
		t.Fatalf("write: %v", cerr)
	}
	_ = w.Close()

	rc, err := root.OpenDecrypted("f.bin")
	if err != nil {
		t.Fatalf("OpenDecrypted: %v", err)
	}
	defer rc.Close()
	src, ok := rc.(SeekReadCloser)
	if !ok {
		t.Fatal("解密流应满足 SeekReadCloser")
	}
	spy := &spyMetaPolicy{}
	rt := runtime{fileMeta: spy}
	got, verr := rt.verifyDownload(context.Background(), root, "f.bin", src)
	if verr != nil || got != nil {
		t.Fatalf("加密卷应返回 (nil,nil)，得到 (%v,%v)", got, verr)
	}
	if spy.called {
		t.Fatal("加密卷不得调用 VerifyDownload（密文 meta vs 明文流必失配）")
	}

	// 非加密对照：VerifyDownload 必须被调用（证明跳过判据只针对加密卷）。
	plain, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot(plain): %v", err)
	}
	defer plain.Close()
	pf, err := plain.OpenFile("p.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, _ = pf.WriteString("plain")
	_ = pf.Close()
	prc, err := plain.Open("p.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer prc.Close()
	spy2 := &spyMetaPolicy{}
	rt2 := runtime{fileMeta: spy2}
	nop, verr2 := rt2.verifyDownload(context.Background(), plain, "p.bin", prc)
	if verr2 != nil || nop == nil {
		t.Fatalf("非加密卷应调用 VerifyDownload，得到 (%v,%v)", nop, verr2)
	}
	if !spy2.called {
		t.Fatal("非加密卷必须调用 VerifyDownload")
	}
}
