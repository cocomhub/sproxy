// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// encrypted_root.go 是 at-rest 加密卷根（roadmap P2 at-rest 加密残余：
// volumes[].extra.encrypt 装配开关）：
//
//   - EncryptedRoot 透明包装 storage.Root：Open 返回解密 reader、OpenFile 写入
//     加密 writer；目录/元数据操作转发。
//   - 密文格式（`[magic][4B chunk]` + 逐块 `[4B len][nonce|ct|tag]`）的**唯一实现**在
//     pkg/cipherstream（与 pkg/sync.EncryptedFS、归档 cipher、客户端 E2EE 共用）。本文件只
//     固定**加密卷的魔数**（域分隔：同一份密文不会被其它密钥域误读）+ 透明包装语义。
//
// 历史（2026-11-08 收敛）：本文件曾自带 writer/reader/SeekPlain 各一份（与 cipherstream 同族
// 实现），靠「源码必须逐字一致」的源码级门禁防分叉——那是重复的症状。现已改为共享实现，
// 随机访问（明文偏移定位）与尺寸探测能力上移到 pkg/cipherstream/random_access.go。

import (
	"fmt"
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/cipherstream"
)

// encRootMagic 是加密卷根魔数头（与 pkg/sync.EncryptedFS 同值 ⇒ **同一密钥域**，两侧密文互通；
// 由 pkg/sync 的跨包互解用例行为锁定）。
var encRootMagic = []byte("SPROXY-FS-ENC-v1")

// encRootCfg 返回加密卷根使用的 cipherstream 配置。
//
// 读侧**不**校验块大小（`VerifyChunkSize=false`：以密文头部声明为准）——与历史行为一致，
// 兼容早期不同块大小写出的既有密文。
func encRootCfg(chunk int) cipherstream.Config {
	if chunk <= 0 {
		chunk = cipherstream.DefaultChunkSize
	}
	return cipherstream.Config{Magic: encRootMagic, ChunkSize: chunk, Label: "storage"}
}

// EncryptedRoot 是透明加密 storage.Root 包装。
type EncryptedRoot struct {
	inner *Root
	key   []byte
}

// NewEncryptedRoot 构造加密卷根包装（inner 为底层 Root；key 必须 32 字节）。
func NewEncryptedRoot(inner *Root, key []byte) (*EncryptedRoot, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: 加密卷 key 必须 32 字节")
	}
	return &EncryptedRoot{inner: inner, key: key}, nil
}

// Close 转发。
func (er *EncryptedRoot) Close() error { return er.inner.Close() }

// AbsPath 转发（物理路径，供元数据引用）。
func (er *EncryptedRoot) AbsPath() string { return er.inner.AbsPath() }

// Abs 转发（路径解析）。
func (er *EncryptedRoot) Abs(rel string) (string, bool) { return er.inner.Abs(rel) }

// ReadFile 读取并解密（透明）。
func (er *EncryptedRoot) ReadFile(rel string) ([]byte, error) {
	f, err := er.inner.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dr, err := cipherstream.NewReader(er.key, f, encRootCfg(0))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(dr)
}

// WriteFile 加密写（透明，流式：io.Pipe 加密 goroutine，底层 FS 流式读）。
func (er *EncryptedRoot) WriteFile(rel string, data []byte, perm os.FileMode) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		ew, werr := cipherstream.NewWriter(er.key, pw, encRootCfg(0))
		if werr != nil {
			_ = pw.CloseWithError(werr)
			errCh <- werr
			return
		}
		if _, cerr := ew.Write(data); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		if cerr := ew.Close(); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		errCh <- pw.Close()
	}()
	// 底层用 OpenFile(O_CREATE|O_TRUNC|O_WRONLY) 流式写 pipe。
	// 中间目录可能不存在（与 os.Root.WriteFile 同语义：MkdirAll(dir)）。
	if d := dirOf(rel); d != "" {
		if merr := er.inner.MkdirAll(d, 0o755); merr != nil {
			_ = pr.CloseWithError(merr)
			return merr
		}
	}
	f, oerr := er.inner.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if oerr != nil {
		_ = pr.CloseWithError(oerr)
		return oerr
	}
	_, cerr := io.Copy(f, pr)
	clerr := f.Close()
	encErr := <-errCh
	if cerr != nil {
		return cerr
	}
	if clerr != nil {
		return clerr
	}
	return encErr
}

// dirOf 返回 rel 的父目录（无分隔符返回空）。
func dirOf(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' || rel[i] == '\\' {
			return rel[:i]
		}
	}
	return ""
}
