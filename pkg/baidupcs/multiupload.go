// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs/pcserror"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/multipartreader"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/rio"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/uploader"
)

// baiduMultiUpload 实现 uploader.MultiUpload 接口（Precreate/TmpFile/CreateSuperFile），
// 把上游多线程分片上传器（MultiUploader）接到百度网盘 API。
//
// 与上游 CLI 层（internal/pcsfunctions/pcsupload）同构，但：
//   - 不使用上游全局 client（pcsconfig）——每实例自建上传客户端（隔离连接池）
//   - 不依赖 UploadingDatabase（断点持久化由本包 Layout.Resume 承担）
type baiduMultiUpload struct {
	pcs        *Client
	targetPath string
}

// newBaiduMultiUpload 构造 MultiUpload 实现。
func newBaiduMultiUpload(pcs *Client, targetPath string) *baiduMultiUpload {
	return &baiduMultiUpload{pcs: pcs, targetPath: targetPath}
}

// uploadClient 是分片上传专用 HTTP 客户端（正文传输无整体超时，由 ctx 约束）。
// 每实例独立连接池（禁共享 http.DefaultClient——仓库硬规则）。
func (u *baiduMultiUpload) uploadClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// Precreate 上传前准备：随机取一个 PCS 服务器（返回原始地址供 CreateSuperFile 恢复）。
// 与上游 PCSUpload.Precreate 同构。
func (u *baiduMultiUpload) Precreate() (string, pcserror.Error) {
	pcs := u.pcs.PCS()
	originHost := pcs.GetPCSAddr()
	_, newHost := pcs.GetRandomPCSHost()
	pcs.SetPCSAddr(newHost)
	return originHost, nil
}

// TmpFile 上传单个分片。uploadURL 由 BaiduPCS 的 PrepareUploadSuperfile2 生成。
func (u *baiduMultiUpload) TmpFile(ctx context.Context, uploadID, targetPath string, partSeq int, partOffset int64, r rio.ReaderLen64) (string, error) {
	pcs := u.pcs.PCS()
	md5, pcsErr := pcs.UploadTmpFile(uploadID, targetPath, partSeq, partOffset, func(uploadURL string, jar http.CookieJar) (*http.Response, error) {
		mr := multipartreader.NewMultipartReader()
		mr.AddFormFile("uploadedfile", "", r)
		_ = mr.CloseMultipart()

		doneChan := make(chan struct{}, 1)
		var (
			resp *http.Response
			err  error
		)
		go func() {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, mr)
			if reqErr != nil {
				err = reqErr
				doneChan <- struct{}{}
				return
			}
			req.Header.Set("Content-Type", mr.ContentType())
			req.Header.Set("User-Agent", "BaiduPCS-Go")
			req.ContentLength = mr.Len()
			cli := u.uploadClient(jar)
			resp, err = cli.Do(req)
			doneChan <- struct{}{}
		}()
		select {
		case <-ctx.Done():
			if resp != nil {
				_ = resp.Body.Close()
			}
			return resp, ctx.Err()
		case <-doneChan:
			return resp, err
		}
	})
	if pcsErr != nil {
		return "", fmt.Errorf("baidupcs: upload tmp file part %d: %w", partSeq, pcsErr)
	}
	return md5, nil
}

// CreateSuperFile 合并全部分片（恢复原始 PCS 地址）。
func (u *baiduMultiUpload) CreateSuperFile(pcsHost, policy, uploadID string, fileSize int64, checksumMap map[int]string) error {
	pcs := u.pcs.PCS()
	pcs.SetPCSAddr(pcsHost) // 恢复默认 PCS 服务器
	if err := pcs.UploadCreateSuperFile(uploadID, policy, fileSize, u.targetPath, checksumMap); err != nil {
		return fmt.Errorf("baidupcs: create super file: %w", err)
	}
	return nil
}

// uploadViaMultiUploader 用上游 MultiUploader 上传本地文件（分片 + 断点）。
// resumeKey 是断点状态在 Layout.Resume 下的标识（空 = 不持久化断点）。
func uploadViaMultiUploader(ctx context.Context, pcs *Client, localPath, targetPath string, overwrite bool, resumeKey string) error {
	file, err := openLocalFile(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	policy := "skip"
	if overwrite {
		policy = "overwrite"
	}

	mu := newBaiduMultiUpload(pcs, targetPath)
	muer := uploader.NewMultiUploader(mu, rio.NewFileReaderAtLen64(file), &uploader.MultiUploaderConfig{
		Parallel:  4,
		BlockSize: 4 * 1024 * 1024,
		MaxRate:   0,
		Policy:    policy,
	}, targetPath)

	// 断点恢复（resumeKey 非空时查本地 Layout.Resume）。
	if resumeKey != "" {
		if st, loadErr := loadUploadResume(resumeKey); loadErr == nil {
			muer.SetInstanceState(st)
		}
	} else if muer.InstanceState() == nil {
		muer.SetInstanceState(&uploader.InstanceState{})
	}

	muer.Execute()

	// 执行完持久化最新断点（供失败恢复）或删除（成功）。
	if resumeKey != "" {
		if err := saveUploadResume(resumeKey, muer.InstanceState()); err != nil {
			return fmt.Errorf("baidupcs: save resume %q: %w", resumeKey, err)
		}
	}
	_ = ctx
	return nil
}
