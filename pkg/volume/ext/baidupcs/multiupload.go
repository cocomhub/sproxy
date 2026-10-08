// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"

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
//
// C-C2 并发修复：共享 `*Client`（libraryAdapter 持单实例，sync 并发 3 转存同卷）下，
// Precreate 对 pcsAddr 做 Set/恢复、TmpFile 经 generatePCSURL 读 pcsAddr——并发上传
// 交错会把分片发往错误 PCS 主机（数据竞争，-race 必报）。uploadMu（libraryAdapter
// 每实例一把，跨上传会话共享）串行化 Precreate↔CreateSuperFile 生命周期：锁在
// Precreate 获取、CreateSuperFile 恢复后释放，中间 TmpFile 并发分片读稳定 host 不受锁。
type baiduMultiUpload struct {
	pcs        *Client
	targetPath string
	uploadMu   *sync.Mutex // 共享上传锁（保护 host 生命周期；nil = 无并发保护旧行为）
	held       bool
}

// newBaiduMultiUpload 构造 MultiUpload 实现。uploadMu 为库 adapter 的共享上传锁
// （防并发 host 竞态）；nil = 单会话无并发（测试 fake 场景）。
func newBaiduMultiUpload(pcs *Client, targetPath string, uploadMu *sync.Mutex) *baiduMultiUpload {
	return &baiduMultiUpload{pcs: pcs, targetPath: targetPath, uploadMu: uploadMu}
}

// uploadClient 是分片上传专用 HTTP 客户端（正文传输无整体超时，由 ctx 约束）。
// 每实例独立连接池（禁共享 http.DefaultClient——仓库硬规则）。
func (u *baiduMultiUpload) uploadClient(jar http.CookieJar) *http.Client {
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Jar:       jar,
		Transport: tr,
	}
}

// Precreate 上传前准备：随机取一个 PCS 服务器（返回原始地址供 CreateSuperFile 恢复）。
// 与上游 PCSUpload.Precreate 同构。
// C-C2：上传会话全程持共享锁（CreateSuperFile 恢复后释放）——防并发上传交错 host。
func (u *baiduMultiUpload) Precreate() (string, pcserror.Error) {
	if u.uploadMu != nil {
		u.uploadMu.Lock()
		u.held = true
	}
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

		// C-M1 修复：resp/err 经 channel 单点发布（取消分支不触碰共享变量——原 goroutine
		// 写 resp/err 与主 select 的 ctx.Done 分支读 resp 并发，-race 数据竞争；HTTP 请求
		// 已随 ctx 取消终止，取消分支只返回 ctx.Err() 不读 resp）。
		type result struct {
			resp *http.Response
			err  error
		}
		done := make(chan result, 1)
		go func() {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, mr)
			if reqErr != nil {
				done <- result{err: reqErr}
				return
			}
			req.Header.Set("Content-Type", mr.ContentType())
			req.Header.Set("User-Agent", "BaiduPCS-Go")
			req.ContentLength = mr.Len()
			cli := u.uploadClient(jar)
			resp, err := cli.Do(req)
			done <- result{resp: resp, err: err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-done:
			return r.resp, r.err
		}
	})
	if pcsErr != nil {
		return "", fmt.Errorf("baidupcs: upload tmp file part %d: %w", partSeq, pcsErr)
	}
	return md5, nil
}

// CreateSuperFile 合并全部分片（恢复原始 PCS 地址）。
// C-C2：恢复地址后释放共享锁（上传会话结束）。
func (u *baiduMultiUpload) CreateSuperFile(pcsHost, policy, uploadID string, fileSize int64, checksumMap map[int]string) error {
	pcs := u.pcs.PCS()
	pcs.SetPCSAddr(pcsHost) // 恢复默认 PCS 服务器
	defer func() {
		if u.held {
			u.uploadMu.Unlock()
			u.held = false
		}
	}()
	if err := pcs.UploadCreateSuperFile(uploadID, policy, fileSize, u.targetPath, checksumMap); err != nil {
		return fmt.Errorf("baidupcs: create super file: %w", err)
	}
	return nil
}

// uploadViaMultiUploader 用上游 MultiUploader 上传本地文件（分片 + 断点）。
// resumeKey 是断点状态在 Layout.Resume 下的标识（空 = 不持久化断点）。
// uploadMu 为共享上传锁（libraryAdapter 每实例一把，跨上传会话串行化 host 生命周期）。
func uploadViaMultiUploader(ctx context.Context, pcs *Client, uploadMu *sync.Mutex, localPath, targetPath string, overwrite bool, resumeKey string) error {
	file, err := openLocalFile(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	policy := "skip"
	if overwrite {
		policy = "overwrite"
	}

	mu := newBaiduMultiUpload(pcs, targetPath, uploadMu)
	muer := uploader.NewMultiUploader(mu, rio.NewFileReaderAtLen64(file), &uploader.MultiUploaderConfig{
		Parallel:  4,
		BlockSize: 4 * 1024 * 1024,
		MaxRate:   0,
		Policy:    policy,
	}, targetPath)

	// 断点恢复（resumeKey 非空时查本地 Layout.Resume）。
	// 修复：不调用上游 muer.InstanceState()——其实现解引用 muer.instanceState，而
	// Execute 前该字段未初始化（nil）→ 真实分片上传 panic（单测用 fake 未暴露）。
	// resumeKey 空直接设置空 state；非空加载失败也设置空 state（从零开始）。
	if resumeKey != "" {
		if st, loadErr := loadUploadResume(resumeKey); loadErr == nil {
			muer.SetInstanceState(st)
		} else {
			muer.SetInstanceState(&uploader.InstanceState{})
		}
	} else {
		muer.SetInstanceState(&uploader.InstanceState{})
	}

	// A-MAJOR-4 修复：MultiUploader.Execute 错误只进 onErrorEvent 不返回——注册 onError
	// 上抛 + ctx 取消经 Cancel 中断在途分片上传（否则用户取消 Put 不终止、网络失败被吞，
	// 靠后续 Stat 复核兜底但代价是整文件重传）。uploadErr 通道收集首错。
	uploadErr := make(chan error, 1)
	muer.OnError(func(err error) {
		select {
		case uploadErr <- err:
		default:
		}
	})
	go func() {
		<-ctx.Done()
		muer.Cancel()
	}()

	muer.Execute()

	select {
	case err := <-uploadErr:
		return fmt.Errorf("baidupcs: 分片上传失败: %w", err)
	default:
	}

	// 执行完持久化最新断点（供失败恢复）或删除（成功）。
	if resumeKey != "" {
		if err := saveUploadResume(resumeKey, muer.InstanceState()); err != nil {
			return fmt.Errorf("baidupcs: save resume %q: %w", resumeKey, err)
		}
	}
	return nil
}
