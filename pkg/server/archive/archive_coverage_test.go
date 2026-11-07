// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archive

// archive_coverage_test.go 补齐 archive.go 的错误/边界分支（包内白盒）：
// 深度上限、Lstat 失败、单文件超限、子条目失败告警、tar 条目名越界、
// WriteHeader/io.Copy 失败、加密卷明文长度 header、TOCTOU 交叉验证拒绝、
// FileInfoHeader 失败。
//
// 直接调用 writeArchiveFile / addFileToTarDepth 的两个用例是**故意**的白盒注入：
// 「Lstat 与 open 观察到不同对象」「文件类型非普通/目录/链接」在真实文件系统上
// 无法稳定构造（只能靠竞态或特权设备节点），而这两个分支正是安全/健壮性守卫，
// 必须被测试钉住。

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

const (
	testUserDir  = "user"
	testTarRoot  = "pkg"
	testNoteFile = "notes.txt"
)

// errWriteSink 是测试用「写入失败」的目标流错误。
var errWriteSink = errors.New("测试写入流失败")

// newArchiveTestRoot 建临时 storage.Root（真实卷根 + t.TempDir），返回根与底层绝对目录。
func newArchiveTestRoot(t *testing.T) (*storage.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// writeCoverageFile 在底层目录写入普通文件（自动建中间目录）。
func writeCoverageFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// writeSparseCoverageFile 用 Truncate 造「逻辑超大、物理稀疏」的文件，
// 避免为触发 100MB 上限真的写 100MB 数据。
func writeSparseCoverageFile(t *testing.T, dir, rel string, size int64) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", rel, err)
	}
	if truncErr := f.Truncate(size); truncErr != nil {
		f.Close()
		t.Fatalf("truncate %s: %v", rel, truncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("close %s: %v", rel, closeErr)
	}
}

// writeEncryptedCoverageFile 经加密 writer 写入明文（落盘为密文，读取需透明解密）。
func writeEncryptedCoverageFile(t *testing.T, root *storage.Root, rel, plaintext string) {
	t.Helper()
	if parent := filepath.ToSlash(filepath.Dir(rel)); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", parent, err)
		}
	}
	wc, err := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFileEncrypted %s: %v", rel, err)
	}
	if _, wErr := io.WriteString(wc, plaintext); wErr != nil {
		_ = wc.Close()
		t.Fatalf("write %s: %v", rel, wErr)
	}
	if cErr := wc.Close(); cErr != nil {
		t.Fatalf("close %s: %v", rel, cErr)
	}
}

// tarEntryNames 读完 tar 流并返回条目名集合（内容被丢弃）。
func tarEntryNames(t *testing.T, r io.Reader) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names[h.Name] = true
	}
}

// tarBlockSize 是 tar 记录块大小（archive/tar 未导出 blockSize）。
const tarBlockSize = 512

// failOnPartialBlock 只放行空写入与整块（512B）写入：tar header 写入成功后，
// 文件内容（非整块）写入失败，用于覆盖 io.Copy 的错误分支。
type failOnPartialBlock struct {
	calls int
}

func (w *failOnPartialBlock) Write(p []byte) (int, error) {
	w.calls++
	if len(p) > 0 && len(p) != tarBlockSize {
		return 0, errWriteSink
	}
	return len(p), nil
}

// fakeFileInfo 模拟「Lstat 观察到的文件信息」，用于注入 TOCTOU 不一致与
// 不支持的 tar 条目类型（真实文件系统上不可稳定构造）。
type fakeFileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// TestAddFileToTarDepth_ExceedsLimit 递归深度超过 100 层直接拒绝。
func TestAddFileToTarDepth_ExceedsLimit(t *testing.T) {
	t.Parallel()
	root, _ := newArchiveTestRoot(t)
	var buf bytes.Buffer
	err := addFileToTarDepth(tar.NewWriter(&buf), root, testUserDir, testTarRoot, slog.New(slog.DiscardHandler), 101)
	if err == nil || !strings.Contains(err.Error(), "目录深度超过限制") {
		t.Fatalf("depth 101 err = %v，want 目录深度超过限制", err)
	}
}

// TestAddFileToTar_LstatFailure 源路径不存在 → stat 失败。
func TestAddFileToTar_LstatFailure(t *testing.T) {
	t.Parallel()
	root, _ := newArchiveTestRoot(t)
	var buf bytes.Buffer
	err := AddFileToTar(tar.NewWriter(&buf), root, "missing.txt", "missing.txt", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "stat 失败") {
		t.Fatalf("err = %v，want stat 失败", err)
	}
}

// TestAddFileToTar_SingleFileOverLimit 单文件超过 100MB 上限 → 拒绝。
func TestAddFileToTar_SingleFileOverLimit(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeSparseCoverageFile(t, dir, testUserDir+"/big.bin", DefaultMaxArchiveSize+1)

	var buf bytes.Buffer
	err := AddFileToTar(tar.NewWriter(&buf), root, testUserDir+"/big.bin", testTarRoot+"/big.bin", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "超过归档限制") {
		t.Fatalf("err = %v，want 超过归档限制", err)
	}
}

// TestAddFileToTar_ChildFailureLoggedAndSkipped 目录内单个文件失败只告警并继续：
// 超限文件被跳过，同目录的普通文件仍进入归档。
func TestAddFileToTar_ChildFailureLoggedAndSkipped(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/"+testNoteFile, "ok")
	writeSparseCoverageFile(t, dir, testUserDir+"/big.bin", DefaultMaxArchiveSize+1)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := AddFileToTar(tw, root, testUserDir, testTarRoot, logger); err != nil {
		t.Fatalf("AddFileToTar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	names := tarEntryNames(t, &buf)
	if !names[testTarRoot+"/"+testNoteFile] {
		t.Fatalf("归档缺少正常文件，条目 = %v", names)
	}
	if names[testTarRoot+"/big.bin"] {
		t.Fatalf("超限文件不应进入归档，条目 = %v", names)
	}
	if !strings.Contains(logs.String(), "归档添加子文件失败") {
		t.Fatalf("缺少子条目失败告警，日志 = %q", logs.String())
	}
}

// TestAddFileToTar_TarNameEscapeRejected tarRel 为 ../ 开头 → 条目名越界拒绝。
func TestAddFileToTar_TarNameEscapeRejected(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/"+testNoteFile, "abc")

	var buf bytes.Buffer
	err := AddFileToTar(tar.NewWriter(&buf), root, testUserDir+"/"+testNoteFile, "../escape.txt", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "归档路径越界") {
		t.Fatalf("err = %v，want 归档路径越界", err)
	}
}

// TestAddFileToTar_WriteHeaderFailure tar writer 已关闭 → 写 header 失败。
func TestAddFileToTar_WriteHeaderFailure(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/"+testNoteFile, "abc")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := AddFileToTar(tw, root, testUserDir+"/"+testNoteFile, testTarRoot+"/"+testNoteFile, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "写入 tar header 失败") {
		t.Fatalf("err = %v，want 写入 tar header 失败", err)
	}
}

// TestAddFileToTar_CopyFailure header 写入成功、内容写入失败 → 内容错误分支。
func TestAddFileToTar_CopyFailure(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/"+testNoteFile, "abc")
	// 整秒 mtime ⇒ USTAR 单块 header（无 PAX 扩展头），保证失败点落在内容写入。
	whole := time.Unix(1700000000, 0)
	if err := root.Chtimes(testUserDir+"/"+testNoteFile, whole, whole); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	sink := &failOnPartialBlock{}
	err := AddFileToTar(tar.NewWriter(sink), root, testUserDir+"/"+testNoteFile, testTarRoot+"/"+testNoteFile, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "写入文件内容失败") {
		t.Fatalf("err = %v（写入次数 %d），want 写入文件内容失败", err, sink.calls)
	}
}

// TestAddFileToTar_EncryptedVolumePlaintextHeader 加密卷：Lstat 是密文长度，
// tar header 必须用打开后的明文长度，内容为用户可见明文。
func TestAddFileToTar_EncryptedVolumePlaintextHeader(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := root.SetEncryption(key); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	const plaintext = "hello encrypted"
	writeEncryptedCoverageFile(t, root, testUserDir+"/secret.txt", plaintext)

	rawInfo, err := os.Stat(filepath.Join(dir, testUserDir, "secret.txt"))
	if err != nil {
		t.Fatalf("stat ciphertext: %v", err)
	}
	if rawInfo.Size() <= int64(len(plaintext)) {
		t.Fatalf("密文长度 %d 应大于明文长度 %d", rawInfo.Size(), len(plaintext))
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if aErr := AddFileToTar(tw, root, testUserDir+"/secret.txt", testTarRoot+"/secret.txt", slog.New(slog.DiscardHandler)); aErr != nil {
		t.Fatalf("AddFileToTar: %v", aErr)
	}
	if cErr := tw.Close(); cErr != nil {
		t.Fatalf("close: %v", cErr)
	}

	tr := tar.NewReader(&buf)
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if h.Size != int64(len(plaintext)) {
		t.Fatalf("tar 条目 size = %d，want 明文长度 %d", h.Size, len(plaintext))
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read tar entry: %v", err)
	}
	if string(got) != plaintext {
		t.Fatalf("tar 条目内容 = %q，want %q", got, plaintext)
	}
}

// TestWriteArchiveFile_RejectsReplacedFile TOCTOU 交叉验证：Lstat 与 open 观察到
// 不同对象（用伪造 FileInfo 注入）→ 非加密卷拒绝归档。
func TestWriteArchiveFile_RejectsReplacedFile(t *testing.T) {
	t.Parallel()
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/"+testNoteFile, "abc")

	replaced := fakeFileInfo{name: testNoteFile, size: 3, mode: 0o644}
	var buf bytes.Buffer
	err := writeArchiveFile(tar.NewWriter(&buf), root, replaced, testUserDir+"/"+testNoteFile, testTarRoot+"/"+testNoteFile)
	if err == nil || !strings.Contains(err.Error(), "TOCTOU") {
		t.Fatalf("err = %v，want TOCTOU 拒绝", err)
	}
}

// TestWriteArchiveFile_HeaderFailure 不支持的条目类型（socket）→ FileInfoHeader 失败。
// 加密卷绕过 TOCTOU 拒绝，使执行流抵达 header 构造；伪造 size 与明文长度一致，
// 保证 header 取的是伪造 FileInfo。
func TestWriteArchiveFile_HeaderFailure(t *testing.T) {
	t.Parallel()
	root, _ := newArchiveTestRoot(t)
	if err := root.SetEncryption(make([]byte, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	writeEncryptedCoverageFile(t, root, testUserDir+"/sock", "abc")

	socket := fakeFileInfo{name: "sock", size: 3, mode: os.ModeSocket | 0o644}
	var buf bytes.Buffer
	err := writeArchiveFile(tar.NewWriter(&buf), root, socket, testUserDir+"/sock", testTarRoot+"/sock")
	if err == nil || !strings.Contains(err.Error(), "创建 tar header 失败") {
		t.Fatalf("err = %v，want 创建 tar header 失败", err)
	}
}
