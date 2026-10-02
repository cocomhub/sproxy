// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"io"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// cleanupReadCloser 是解密读的 ReadCloser：Close 时同时删除临时解密文件。
type cleanupReadCloser struct {
	rc   io.ReadCloser
	path string
}

func (c *cleanupReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *cleanupReadCloser) Close() error {
	err := c.rc.Close()
	_ = os.Remove(c.path)
	return err
}

// encryptContent 把明文内容分块加密写到 outDir，返回分块与 meta 信息。
// 走 shardseal.EncryptShards：临时源文件用**逻辑名**命名（EncryptShards 以
// filepath.Base(srcFile) 作为 meta.original.Name——若用 `.src-` 前缀会污染
// meta 的原始文件名，旧卷加载时会以 `.src-xxx` 建逻辑键）。padTarget 透传给
// EncryptShards（文件 meta 加密 padding 目标整块落盘总长；secretdata 卷传 metaPadTarget）。
// v 是算法版本（NewFS 由 Options.Algorithm 解析出的已注册版本，写路径不硬编码）。
func encryptContent(content string, outDir string, secret []byte, policy shardseal.BlockPolicy, name string, padTarget int, v shardseal.AlgoVersion) (*shardseal.EncryptionResult, error) {
	tmpSrc := filepath.Join(outDir, sanitizeName(name))
	if err := os.WriteFile(tmpSrc, []byte(content), 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(tmpSrc)
	return shardseal.EncryptShards(tmpSrc, outDir, secret, policy, padTarget, v)
}

// sanitizeName 把逻辑文件名归一为安全文件名（临时源文件用）。
func sanitizeName(name string) string {
	base := filepath.Base(name)
	if base == "." || base == "" {
		return "file"
	}
	return base
}
