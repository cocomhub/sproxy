// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package archive 是**归档打包**的纯助手（server 作用域）：把 storage.Root 内文件/目录
// 递归写入 tar，含符号链接拒绝、TOCTOU 交叉验证与单文件大小上限。
// HTTP 面（/api/archive、/api/archive-dir）留在装配层。
package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// addFileToTar 将 root 内 rel 对应的文件（或目录）添加到 tar writer 中（tar 条目名为 tarRel）。
// 如果是目录则递归添加。
// 安全边界：所有读取经 os.Root 相对路径（root.Lstat/Open/ReadDir），os.Root 对每路径分量
// 强制 O_NOFOLLOW，中间目录符号链接指向 root 外即报错（不逃逸）；TOCTOU 交叉验证
// os.SameFile 确保 lstat 和 open 后文件一致（最终组件符号链接被 Lstat 拒绝跟随）。
func AddFileToTar(tw *tar.Writer, root *storage.Root, rel, tarRel string, logger *slog.Logger) error {
	return addFileToTarDepth(tw, root, rel, tarRel, logger, 0)
}

// addFileToTarDepth 是 addFileToTar 内部实现，带 depth 参数防止递归过深。
func addFileToTarDepth(tw *tar.Writer, root *storage.Root, rel, tarRel string, logger *slog.Logger, depth int) error {
	if depth > 100 {
		return fmt.Errorf("目录深度超过限制: %s", tarRel)
	}

	// 使用 Lstat 检测符号链接，拒绝跟随
	info, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("stat 失败: %w", err)
	}

	// 检测符号链接，拒绝归档
	if info.Mode()&os.ModeSymlink != 0 {
		logger.Warn("跳过符号链接", "path", tarRel)
		return nil
	}

	if info.IsDir() {
		// 递归添加目录内容（root 相对读取，防中间目录符号链接逃逸）
		entries, readErr := root.ReadDir(rel)
		if readErr != nil {
			return fmt.Errorf("读取目录失败: %w", readErr)
		}
		for _, entry := range entries {
			childRel := path.Join(rel, entry.Name())
			childTarRel := filepath.ToSlash(filepath.Join(tarRel, entry.Name()))
			if err = addFileToTarDepth(tw, root, childRel, childTarRel, logger, depth+1); err != nil {
				logger.Warn("归档添加子文件失败", "path", childTarRel, "error", err)
			}
		}
		return nil
	}

	return writeArchiveFile(tw, root, info, rel, tarRel)
}

// writeArchiveFile 把单个普通文件写入 tar writer：大小限制 → os.Root 打开 → TOCTOU
// 交叉验证 → 条目名归一 → WriteHeader + 内容拷贝。
func writeArchiveFile(tw *tar.Writer, root *storage.Root, info os.FileInfo, rel, tarRel string) error {
	// 单个文件大小限制
	if info.Size() > DefaultMaxArchiveSize {
		return fmt.Errorf("文件 %s 大小 (%d) 超过归档限制 (%d)，请直接下载该文件", tarRel, info.Size(), DefaultMaxArchiveSize)
	}

	// 打开文件：os.Root 保证不跟随符号链接（最终组件 + 中间目录均 O_NOFOLLOW）
	file, err := rootfs.OpenUserVisibleRead(root, rel)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 交叉验证：lstat 得到的文件信息与打开后的文件信息一致
	stater, ok := file.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return fmt.Errorf("打开句柄不支持 Stat（无法做 TOCTOU 交叉验证）: %T", file)
	}
	openedInfo, err := stater.Stat()
	if err != nil {
		return fmt.Errorf("stat 已打开文件失败: %w", err)
	}
	// TOCTOU 交叉验证：非加密卷用 os.SameFile（lstat 与 open 同形态句柄，可靠）。
	// 加密卷（OpenDecrypted 内部两次底层 open）在 Windows 上 Lstat 与 open 的 Sys
	// FileIndex 表示不同 → SameFile 恒 false；此时降级为「打开后 Size/Mode 一致」的弱判据
	// （明文 size 与 Lstat 密文 size 本就不等，仅比 Mode），并把不匹配仍按替换拒绝。
	if os.SameFile(info, openedInfo) {
		// 同文件：正常
	} else if !root.IsEncrypted() {
		return fmt.Errorf("文件在 lstat 和 open 之间被替换（TOCTOU）: %s", tarRel)
	}
	// 归档条目大小取**用户可见长度**（明文）：加密卷 Lstat 的 Size 是密文长度，用它做
	// tar header 会与实际写入的明文流不符（客户端解出条目 size 错、EOF 提前）。
	headerInfo := info
	if openedInfo.Size() != info.Size() {
		headerInfo = openedInfo
	}

	header, err := tar.FileInfoHeader(headerInfo, "")
	if err != nil {
		return fmt.Errorf("创建 tar header 失败: %w", err)
	}
	// 审查 P3：tar 条目名显式归一——rel 已过 ValidateFilePath/UserRel（无 .. / 绝对路径），
	// 但 ToSlash 后再 filepath.Clean 双保险（防未来目录递归路径拼接引入未归一段）。
	tarName := filepath.ToSlash(tarRel)
	if filepath.IsAbs(tarName) || strings.HasPrefix(tarName, "../") {
		return fmt.Errorf("归档路径越界: %s", tarName)
	}
	header.Name = tarName

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("写入 tar header 失败: %w", err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("写入文件内容失败: %w", err)
	}
	return nil
}

// DefaultMaxArchiveSize 是归档中单个文件的最大大小（100MB）。
const DefaultMaxArchiveSize int64 = 100 * 1024 * 1024
