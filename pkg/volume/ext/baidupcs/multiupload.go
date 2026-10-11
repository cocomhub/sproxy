// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs/pcserror"
	"github.com/qjfoidnh/BaiduPCS-Go/requester"
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
// C-C2/C3 并发修复：共享 `*Client`（libraryAdapter 持单实例，sync 并发 3 转存同卷）下，
// Precreate 对 pcsAddr 做 Set/恢复、TmpFile 经 generatePCSURL 读 pcsAddr——并发上传
// 交错会把分片发往错误 PCS 主机（数据竞争，-race 必报）。uploadMu（libraryAdapter
// 每实例一把，跨上传会话共享）串行化 Precreate↔CreateSuperFile 生命周期：**锁在
// uploadViaMultiUploader 的 Execute 入口获取、defer 释放**（C3 修复：原设计只在
// CreateSuperFile 的 defer 释放，ctx 取消/"Terminated"分片错误/Precreate 后返回等
// 上游路径不调 CreateSuperFile → 锁永久 hold 死锁；收敛到 Execute 全程持有，中间
// TmpFile 并发分片读稳定 host 不受锁影响）。
type baiduMultiUpload struct {
	pcs        *Client
	targetPath string
}

// newBaiduMultiUpload 构造 MultiUpload 实现。
func newBaiduMultiUpload(pcs *Client, targetPath string) *baiduMultiUpload {
	return &baiduMultiUpload{pcs: pcs, targetPath: targetPath}
}

// uploadClient 返回分片上传用 HTTP 客户端（性能修复 e2e 实测 2026-10-08）：
// **复用 pcs 底层 requester.HTTPClient**（与 Stat/Precreate/CLI 同 HTTP 栈——50s
// 整体 Timeout + 正确 transport/proxy 配置 + keep-alive 连接复用 + requester.Req
// 自动 UA/Content-Type/ContentLength 处理）。原自建 netutil.IsolatedTransport 每分片
// 新建连接池且无整体超时、裸 http.Client.Do 缺 requester 层 header 处理——百度分片
// 接口挂起（响应 body 无限等待）且分片间零连接复用（C-M2）。jar 参数保留兼容（实际
// 复用 pcs client 已带 cookie jar）。
func (u *baiduMultiUpload) uploadClient(jar http.CookieJar) *requester.HTTPClient {
	// D-MAJOR 修复：移除冗余 SetCookiejar——jar 即 client 自身 cookie jar（fork 库
	// preparePCSHeader 传入的自赋值），并发分片下 SetCookiejar 无锁写 Jar 字段构成
	// W/R 数据竞争（-race 必报）。pcs 默认已带 cookie jar，直接复用即可。
	_ = jar // 兼容参数保留（无实际用途，显式忽略防 future jar 变化）
	return u.pcs.PCS().GetClient()
}

// Precreate 上传前准备：与上游 PCSUpload.Precreate 同构。
// C4 修复：**不再随机切换 PCS host**——原实现 SetPCSAddr(newHost) 写 fork 库裸字段
// pcsAddr，与并发 metaop（Stat/List/Move/Copy/Delete/LocateDownload 读 pcsAddr）构成
// 数据竞争（-race 必报；功能 benign 因随机 host 均合法）。默认 pcs_addr 节点即可用
// （CLI/e2e 同节点），保持恒稳定 host 消除写竞争。返回 originHost（当前 host）供
// CreateSuperFile 恢复兼容（值不变，no-op 语义）。
func (u *baiduMultiUpload) Precreate() (string, pcserror.Error) {
	pcs := u.pcs.PCS()
	return pcs.GetPCSAddr(), nil
}

// TmpFile 上传单个分片。uploadURL 由 BaiduPCS 的 PrepareUploadSuperfile2 生成。
// 性能修复（e2e 实测 2026-10-08）：挂起根因是**请求构造路径差异**——CLI 分片上传用
// `pcsconfig.PCSHTTPClient().Req`（requester 层：自动 User-Agent/Content-Type/ContentLength
// 处理 + keep-alive 连接复用），sproxy 原用裸 `http.Client.Do`（无 requester 层的 UA/
// header 处理 → 百度分片接口挂起）。此处改用 `pcs.GetClient().Req` 与 CLI 完全一致。
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
			// 与 CLI 同构：requester.HTTPClient.Req + **显式空 UA**（百度分片接口对
			// Chrome/Netdisk UA 挂起——CLI 的 preparePCSHeader 用 pcsUA 默认空覆盖；
			// header 传 User-Agent="" 与 CLI 完全一致）。
			cli := u.uploadClient(jar)
			resp, err := cli.Req(http.MethodPost, uploadURL, mr, map[string]string{"User-Agent": ""})
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

// CreateSuperFile 合并全部分片。
// C4 修复：不再恢复 PCS host（Precreate 已不切换——恒默认节点，恢复 no-op）。
func (u *baiduMultiUpload) CreateSuperFile(pcsHost, policy, uploadID string, fileSize int64, checksumMap map[int]string) error {
	pcs := u.pcs.PCS()
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

	mu := newBaiduMultiUpload(pcs, targetPath)
	muer := uploader.NewMultiUploader(mu, rio.NewFileReaderAtLen64(file), &uploader.MultiUploaderConfig{
		Parallel:  4,
		BlockSize: 4 * 1024 * 1024,
		MaxRate:   0,
		Policy:    policy,
	}, targetPath)

	// C3 修复：uploadMu 释放收敛到本函数 defer（Execute 返回即解开）——原设计只在
	// CreateSuperFile 的 defer 释放；上游 MultiUploader 在 ctx 取消/"Terminated"分片
	// 错误/Precreate 后返回等路径**不调用 CreateSuperFile** → 锁被永久 hold，之后该卷
	// 所有上传在 Precreate 的 Lock() 死锁。此处 Execute 全程持有（Precreate host 切换
	// 与 CreateSuperFile 恢复都在锁内），结束统一释放。
	if uploadMu != nil {
		uploadMu.Lock()
		defer uploadMu.Unlock()
	}

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
