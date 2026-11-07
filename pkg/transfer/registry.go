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

// PreprocessorConfig 是预处理器构造参数（类型化，非 map——避免任意键漂移）。
type PreprocessorConfig struct {
	// Compression 压缩算法（调用方应先经 compressx.Parse 归一，默认 gzip）。
	Compression compressx.Algorithm
	// Cipher 加密算法名（空 = defaultCipher）；仅 EncryptKey 非空时生效。
	Cipher string
	// EncryptKey 非 nil（32 字节）= 加密打包（tar.<ext>.aes）；nil = 纯打包。
	EncryptKey []byte
}

// PreprocessorFactory 按类型化配置构造预处理器。
type PreprocessorFactory func(cfg PreprocessorConfig) (Preprocessor, error)

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

// New 按名构造预处理器（未注册 → ErrUnknownPreprocessor）。
func New(name string, cfg PreprocessorConfig) (Preprocessor, error) {
	f := Lookup(name)
	if f == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPreprocessor, name)
	}
	return f(cfg)
}

// init 注册内置打包预处理器（tar：压缩 + 可选加密）——使 registry 成为「预处理选型」
// 的唯一入口（服务端经 New("tar", cfg) 构造，防直接散建）。
func init() {
	Register("tar", func(cfg PreprocessorConfig) (Preprocessor, error) {
		return &TarPreprocessor{Compression: cfg.Compression, Cipher: cfg.Cipher, EncryptKey: cfg.EncryptKey}, nil
	})
}

// TarPreprocessor 是打包预处理（复用 compressx 压缩 + archive/tar）。
// 把 N 个源文件打包为单一 tar 流（可选压缩算法 gzip/zstd/brotli）。
type TarPreprocessor struct {
	// Compression 压缩算法（gzip 默认；zstd/brotli 扩展）。
	Compression compressx.Algorithm
	// Cipher 加密算法名（空 = aes-256-gcm）；仅 EncryptKey 非空时生效。
	Cipher string
	// EncryptKey 非 nil = 加密打包（tar.gz.aes）；nil = 纯 tar。
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
//
// **收尾错误不得吞**（P1）：tar/压缩/加密 writer 的 Close 会落最后一块数据
// （tar 端标记、gzip/zstd 尾、AES-GCM 末块 tag）——`defer Close()` 丢弃错误会把
// 损坏产物当成功（archive/tar 的「missed writing N bytes」只在 Close 暴露，声明长度
// 与实读不符时必然触发）。故按序显式 Close 并收集首个错误（与
// pkg/server/archive.go 的归档实现同口径）。
func (p *TarPreprocessor) Pack(ctx context.Context, entries []SourceEntry, w io.Writer) error {
	dest, closeDest, err := p.packDest(w)
	if err != nil {
		return err
	}
	gw, err := compressx.NewWriter(p.Compression, dest, 0)
	if err != nil {
		_ = closeDest()
		return fmt.Errorf("transfer: 压缩初始化: %w", err)
	}
	tw := tar.NewWriter(gw)
	var werr error
	for _, e := range entries {
		if werr = writeTarEntry(ctx, tw, e); werr != nil {
			break
		}
	}
	// 按序关闭（tar → 压缩 → 加密）；首个错误为准，但三条 Close 都必须执行以释放资源。
	if cerr := tw.Close(); cerr != nil && werr == nil {
		werr = fmt.Errorf("transfer: tar 收尾: %w", cerr)
	}
	if cerr := gw.Close(); cerr != nil && werr == nil {
		werr = fmt.Errorf("transfer: 压缩收尾: %w", cerr)
	}
	if cerr := closeDest(); cerr != nil && werr == nil {
		werr = fmt.Errorf("transfer: 加密收尾: %w", cerr)
	}
	return werr
}

// packDest 按 EncryptKey 包装加密 writer（nil key = 直写）。返回 (dest, closeDest, err)；
// closeDest 的 **错误必须回传**（加密 writer 在 Close 时落末块与 tag）。
func (p *TarPreprocessor) packDest(w io.Writer) (io.Writer, func() error, error) {
	if len(p.EncryptKey) == 0 {
		return w, func() error { return nil }, nil
	}
	ew, err := newCipherWriter(w, p.Cipher, p.EncryptKey)
	if err != nil {
		return nil, nil, err
	}
	return ew, ew.Close, nil
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
