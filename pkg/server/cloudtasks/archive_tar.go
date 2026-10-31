// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// archive_tar.go 是归档 tar.gz 打包助手（P1-c S4）：把源文件打包为 tar.gz 并流式
// 计算 SHA-256，输出经 storage.Root 的用户可见写入口（O_EXCL 语义）。
//
// 迁入前它是装配层（pkg/server/cloud_archive_handler.go）的私有实现，编排核心经
// Targets.CreateArchiveTar/CreateMultiArchiveTar 两个接口方法回指——S4 把实现搬到
// 消费方本包后接缝删除（同一能力只留一份实现，且不再反向依赖装配层）。
package cloudtasks

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/server/archive"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// errArchiveExists 归档输出文件已存在（O_EXCL 创建失败）。
var errArchiveExists = errors.New("archive file already exists")

// createTarGz 将单个文件打包为 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在返回 errArchiveExists
// （调用方映射为 409）。源文件经 srcRoot 相对 srcRel 读取、输出经 outRoot 相对 outRel 打开
// （os.Root 防符号链接逃逸；O_EXCL 语义保留）。
func createTarGz(srcRoot *storage.Root, srcRel, srcTarName string, outRoot *storage.Root, outRel string, logger *slog.Logger) (checksum string, err error) {
	outputFile, created, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !created {
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// **收尾顺序必须显式**（P1 修复，2026-10-21）：此前 tw/gw 都是 `defer Close()`，而
	// checksum 在 defer 之前就计算 ⇒ ① 哈希漏掉 tar 的 EOF 两块 + gzip 尾（CRC32/ISIZE）
	// ⇒ 响应里的 checksum 100% 不等于落盘文件（客户端按它校验必然判「归档损坏」）；
	// ② Close 的错误被丢弃且 succeeded 已置 true ⇒ 收尾失败的**截断归档**被当成功返回
	// 且不清理。与 pkg/server/archive.go、pkg/transfer/registry.go 的既有口径统一。
	if err := archive.AddFileToTar(tw, srcRoot, srcRel, srcTarName, logger); err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return "", fmt.Errorf("add file to tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// createMultiFileTarGz 将多个文件打包为单个 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在返回 errArchiveExists
// （调用方映射为 409）。源文件按各自租户根（sources[].Root）相对读取、输出经 outRoot 相对
// outRel 打开（os.Root 防符号链接逃逸；O_EXCL 语义保留）。
//
// 入参直接用 ArchiveSource（源文件描述的唯一类型）：迁入前装配层另设同形的
// fileWithRelPath 并为两者写适配函数，本包内两者同形即重复，故只保留 ArchiveSource。
func createMultiFileTarGz(sources []ArchiveSource, outRoot *storage.Root, outRel string, logger *slog.Logger) (checksum string, err error) {
	outputFile, created, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !created {
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// 与 createTarGz 同口径（P1 修复，2026-10-21）：显式收尾 + 收尾后再算 checksum。
	for _, src := range sources {
		if err := archive.AddFileToTar(tw, src.Root, src.Rel, src.TarRel, logger); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return "", fmt.Errorf("add file %q to tar: %w", src.TarRel, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// openArchiveOutput 以 O_EXCL 语义打开归档输出文件（相对租户 root 的 rel 路径）。
// 跨平台统一用 errors.Is(err, os.ErrExist) 判已存在（Windows 上 O_EXCL 对已存在文件同样报错）。
func openArchiveOutput(root *storage.Root, rel string) (io.WriteCloser, bool, error) {
	// 归档输出是**用户内容**写面：加密卷须透明加密落盘（raw OpenFile 写明文，
	// 归档产物读回时按密文形态不完整；评审「用户可见入口」收敛面）。
	f, err := rootfs.WriteUserVisible(root, rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("create archive output: %w", err)
	}
	return f, true, nil
}
