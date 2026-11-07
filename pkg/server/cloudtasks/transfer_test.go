// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/transfer"
)

// 本文件锁 S3c 迁入转存核心的**包内契约**（与 pkg/server 的既有 HTTP 测试互补）：
// 归档桶占用登记状态、转存目标键派生、转存选项校验规则。这三者是「写侧记账 → 删除侧
// 同源释放」「目标键与读路径同键空间」「仅转存外部卷支持的选项不得静默忽略」的唯一实现。

// TestArchiveUsage_RecordSizeRemove 登记表语义：actual<=0 不登记、覆盖写按新值、按 owner
// 隔离、remove 清空（删除归档后释放「按登记」回退的依据）。
func TestArchiveUsage_RecordSizeRemove(t *testing.T) {
	t.Parallel()
	svc := New(Deps{})

	// actual<=0（文件未落盘）不登记 ⇒ 删除时不会做幻影释放。
	svc.recordArchiveUsage("alice", "a.tar.gz", 0)
	if got := svc.ArchiveUsage("alice", "a.tar.gz"); got != 0 {
		t.Fatalf("actual=0 不应登记，got %d", got)
	}

	svc.recordArchiveUsage("alice", "a.tar.gz", 100)
	svc.recordArchiveUsage("alice", "a.tar.gz", 250)
	if got := svc.ArchiveUsage("alice", "a.tar.gz"); got != 250 {
		t.Fatalf("覆盖登记后 = %d，want 250", got)
	}
	// owner 隔离：同名归档在不同 owner 下互不影响。
	svc.recordArchiveUsage("bob", "a.tar.gz", 7)
	if got := svc.ArchiveUsage("bob", "a.tar.gz"); got != 7 {
		t.Fatalf("bob 登记 = %d，want 7", got)
	}
	if got := svc.ArchiveUsage("alice", "a.tar.gz"); got != 250 {
		t.Fatalf("owner 隔离失效：alice = %d，want 250", got)
	}

	svc.usage.remove("alice", "a.tar.gz")
	if got := svc.ArchiveUsage("alice", "a.tar.gz"); got != 0 {
		t.Fatalf("remove 后 = %d，want 0", got)
	}
	// 空 owner 归一 anonymous（与租户名契约同源）。
	svc.recordArchiveUsage("", "b.tar.gz", 9)
	if got := svc.ArchiveUsage("", "b.tar.gz"); got != 9 {
		t.Fatalf("空 owner 应归一 anonymous，got %d", got)
	}
}

// TestArchiveTargetRelAndUserRelFromFSKey 锁目标键派生：默认 archive/<name>.tar.gz
// （已带 .tar.gz / .tar.gz.aes 不重复追加）、目录式 target_path 拼接、显式路径保留；
// 以及删除侧把卷内键 `<owner>/user/<rel>` 还原为用户桶相对路径。
func TestArchiveTargetRelAndUserRelFromFSKey(t *testing.T) {
	t.Parallel()

	relCases := []struct{ targetPath, archiveName, want string }{
		{"", "x", "archive/x.tar.gz"},
		{"", "x.tar.gz", "archive/x.tar.gz"},
		{"", "x.tar.gz.aes", "archive/x.tar.gz.aes"},
		{"/dir/", "x.tar.gz", "dir/x.tar.gz"},
		{"dir/file.tar.gz", "ignored", "dir/file.tar.gz"},
		// 目录式 target_path 分支按原样拼接 archiveName（不追加后缀；调用方传入的是
		// NormalizeArchiveName 归一后的归档名），前后空白已归一。
		{"  /dir/  ", "x", "dir/x"},
	}
	for _, c := range relCases {
		if got := archiveTargetRel(c.targetPath, c.archiveName); got != c.want {
			t.Errorf("archiveTargetRel(%q, %q) = %q, want %q", c.targetPath, c.archiveName, got, c.want)
		}
	}
	// relOf 是 archiveTargetRel 的空 target_path 分支（直接覆盖扩展名规则）。
	if got := relOf("y.tar.gz.aes"); got != "archive/y.tar.gz.aes" {
		t.Errorf("relOf 已带 .aes 后缀时 = %q，不应重复追加", got)
	}

	keyCases := []struct{ owner, fsKey, want string }{
		{"alice", "alice/user/archive/x.tar.gz", "archive/x.tar.gz"},
		{"alice", "user/archive/x.tar.gz", "archive/x.tar.gz"},
		{"alice", "other/user/archive/x.tar.gz", "other/user/archive/x.tar.gz"},
	}
	for _, c := range keyCases {
		if got := userRelFromFSKey(c.owner, c.fsKey); got != c.want {
			t.Errorf("userRelFromFSKey(%q, %q) = %q, want %q", c.owner, c.fsKey, got, c.want)
		}
	}
}

// TestArchiveExtrasError 锁「仅转存外部卷支持」的选项校验（规则唯一实现）：
// 无 target_volume 时其余选项一律 400（防静默忽略），cipher 必须与 encrypt 同用。
func TestArchiveExtrasError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		extras ArchiveExtras
		status int
	}{
		{"bucket only", ArchiveExtras{}, 0},
		{"target without extras", ArchiveExtras{TargetVolume: "vault"}, 0},
		{"target with encrypt", ArchiveExtras{TargetVolume: "vault", Encrypt: true, Cipher: "aes-256-gcm"}, 0},
		{"path without target", ArchiveExtras{TargetPath: "a/b"}, http.StatusBadRequest},
		{"encrypt without target", ArchiveExtras{Encrypt: true}, http.StatusBadRequest},
		{"compression without target", ArchiveExtras{Compression: "zstd"}, http.StatusBadRequest},
		{"cipher without encrypt", ArchiveExtras{TargetVolume: "vault", Cipher: "aes-256-gcm"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := archiveExtrasError(c.extras); got != c.status {
				t.Fatalf("archiveExtrasError(%+v) status = %d, want %d", c.extras, got, c.status)
			}
		})
	}
}

// TestCloseEntries_ClosesAllReaders 转存核心用 defer 关闭全部源 Reader：
// 中途失败路径也经 closeEntries 收口（防句柄泄漏）。
func TestCloseEntries_ClosesAllReaders(t *testing.T) {
	t.Parallel()
	a, b := &countingReadCloser{}, &countingReadCloser{}
	closeEntries([]transfer.SourceEntry{
		{Reader: a}, {Reader: b}, {Reader: strings.NewReader("noop")},
	})
	if a.closed != 1 || b.closed != 1 {
		t.Fatalf("Reader 未全部关闭：a=%d b=%d", a.closed, b.closed)
	}
}

// countingReadCloser 统计 Close 调用次数（io.Closer 断言分支）。
type countingReadCloser struct {
	closed int
}

func (c *countingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *countingReadCloser) Close() error {
	c.closed++
	return nil
}
