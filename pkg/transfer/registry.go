// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/cocomhub/sproxy/pkg/compressx"
)

// 预处理器注册表：按名构造（可插拔，类似 volume/registry）。
// 注册的预处理器经 Register 挂入；nil 名 = individual（内置）。
var (
	mu            sync.RWMutex
	preprocessors = map[string]PreprocessorFactory{}
)

// PreprocessorFactory 按配置构造预处理器（nil 配置 = 默认参数）。
type PreprocessorFactory func(cfg map[string]any) (Preprocessor, error)

// Register 注册预处理器类型。重名 panic（装配期 fail-fast，防静默覆盖）。
func Register(name string, f PreprocessorFactory) {
	if name == "" {
		panic("transfer: 预处理器名不能为空")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := preprocessors[name]; ok {
		panic(fmt.Sprintf("transfer: 预处理器 %q 重复注册", name))
	}
	preprocessors[name] = f
}

// Lookup 按名取预处理器工厂；未注册返回 nil（调用方决定回退/报错）。
func Lookup(name string) PreprocessorFactory {
	mu.RLock()
	defer mu.RUnlock()
	return preprocessors[name]
}

// TarPreprocessor 是打包预处理（复用 compressx 压缩 + archive/tar）。
// 把 N 个源文件打包为单一 tar 流（可选压缩算法 gzip/zstd/brotli）。
type TarPreprocessor struct {
	// Compression 压缩算法（gzip 默认；zstd/brotli 扩展）。
	Compression compressx.Algorithm
	// EncryptKey 非 nil = AES 加密打包（tar.gz.aes）；nil = 纯 tar。
	EncryptKey []byte
}

// Name 返回预处理类型名（tar / tar-encrypt）。
func (p *TarPreprocessor) Name() string {
	if len(p.EncryptKey) > 0 {
		return "tar-encrypt"
	}
	return "tar"
}

// Pack 把 entries 打包为单一 tar 流写到 w。
func (p *TarPreprocessor) Pack(ctx context.Context, entries []SourceEntry, w io.Writer) error {
	dest, cleanup, err := p.packDest(w)
	if err != nil {
		return err
	}
	defer cleanup()
	gw, err := compressx.NewWriter(p.Compression, dest, 0)
	if err != nil {
		return fmt.Errorf("transfer: 压缩初始化: %w", err)
	}
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	for _, e := range entries {
		if err := writeTarEntry(ctx, tw, e); err != nil {
			return err
		}
	}
	return nil
}

// packDest 按 EncryptKey 包装加密 writer（nil key = 直写）。返回 (dest, cleanup)。
func (p *TarPreprocessor) packDest(w io.Writer) (io.Writer, func(), error) {
	if len(p.EncryptKey) == 0 {
		return w, func() {}, nil
	}
	ew, err := newAESWriter(w, p.EncryptKey)
	if err != nil {
		return nil, nil, err
	}
	return ew, func() { ew.Close() }, nil
}

// writeTarEntry 写单个 tar 条目（含 ctx 取消检查 + Reader 非空校验）。
func writeTarEntry(ctx context.Context, tw *tar.Writer, e SourceEntry) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if e.Reader == nil {
		return fmt.Errorf("transfer: 条目 %q 无 Reader（打包需内容流）", e.Rel)
	}
	hdr := &tar.Header{
		Name:    e.Rel,
		Mode:    0o644,
		Size:    e.Size,
		ModTime: unixToTime(e.ModTime),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("transfer: tar header %q: %w", e.Rel, err)
	}
	if _, err := io.Copy(tw, e.Reader); err != nil {
		return fmt.Errorf("transfer: tar 内容 %q: %w", e.Rel, err)
	}
	return nil
}
