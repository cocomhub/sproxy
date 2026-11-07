// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// 下载断点续传（弱网自愈）状态管理。
//
// 背景：此前 `--resume` 对下载完全无效——`WithChunkedResume` 只喂给了上传路径
// （getDownloadParams 从不读取 opt.resume），单流下载与分块下载失败都会删除已收
// 内容，弱网下大文件「每次重来、每次都挂在同一处」。此外隧道整请求 Timeout
// （默认 300s）会在中途掐断长下载（decrypt stream: unexpected EOF）。
//
// 方案：把「已收进度」落成与输出文件同目录的 sidecar（JSON，原子写）。
//   - 单流下载：Offset = 已写入字节，重试用 `Range: bytes=<offset>-` 续传；
//   - 分块下载：Completed = 已完成分块位图（base64），跳过已收分块。
//
// sidecar 记录远端身份（filename/kind/size/checksum/mtime/chunk_size）；重入时
// 任一不匹配即视为「远端已变/不完整」，丢弃续传状态从头开始（fail-safe，绝不
// 把不同文件的字节拼到一个输出里）。下载成功即删除 sidecar；失败保留以便下次续传。

const (
	// downloadResumeVersion 是 sidecar schema 版本；变更格式必须递增。
	downloadResumeVersion = 1
	// downloadResumeSuffix 是 sidecar 文件名后缀（与输出文件同目录）。
	downloadResumeSuffix = ".sproxy-resume.json"
	// downloadStreamMaxAttempts 是单流下载在一次调用内的最大尝试次数（含首次）。
	downloadStreamMaxAttempts = 5
	// downloadResumeChunkFlush 是分块续传位图的落盘节流：每完成 N 块落一次盘。
	downloadResumeChunkFlush = 16
)

// downloadResumeState 是下载断点续传 sidecar 的持久化结构。
type downloadResumeState struct {
	Version int `json:"version"`
	// Filename/Kind 是远端身份（kind 空 = 普通文件下载）。
	Filename string `json:"filename"`
	Kind     string `json:"kind,omitempty"`
	// Size/Checksum/MTime 是远端文件的稳定身份（服务端 Stat 返回）。
	Size     int64  `json:"size"`
	Checksum string `json:"checksum,omitempty"`
	MTime    int64  `json:"mtime,omitempty"`
	// ChunkSize = 分块大小；0 表示单流下载（此时 Offset 生效）。
	ChunkSize int64 `json:"chunk_size,omitempty"`
	// Offset 是单流下载已写入输出文件的字节数。
	Offset int64 `json:"offset"`
	// Completed 是分块下载的已完成位图（base64，低位在前）。
	Completed string `json:"completed,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// downloadResumePath 返回输出文件对应的 sidecar 路径。
func downloadResumePath(outputPath string) string { return outputPath + downloadResumeSuffix }

// downloadResumeMatches 判断 sidecar 是否与当前远端同一文件、同一分块策略。
// checksum/mtime 为空（远端未提供）时跳过对应比较；chunkSize 必须精确匹配。
func (st *downloadResumeState) downloadResumeMatches(filename, kind string, size int64, checksum string, mtime, chunkSize int64) bool {
	if st == nil || st.Version != downloadResumeVersion {
		return false
	}
	if st.Filename != filename || st.Kind != kind || st.Size != size || st.ChunkSize != chunkSize {
		return false
	}
	if checksum != "" && st.Checksum != checksum {
		return false
	}
	if mtime != 0 && st.MTime != 0 && st.MTime != mtime {
		return false
	}
	return true
}

// loadDownloadResumeState 读取 sidecar；缺失/损坏/版本不符返回 (nil,false)。
func loadDownloadResumeState(outputPath string) (*downloadResumeState, bool) {
	b, err := os.ReadFile(downloadResumePath(outputPath))
	if err != nil {
		return nil, false
	}
	var st downloadResumeState
	if err := json.Unmarshal(b, &st); err != nil || st.Version != downloadResumeVersion {
		return nil, false
	}
	return &st, true
}

// writeDownloadResumeState 原子落盘 sidecar（同目录 tmp + rename）。
func writeDownloadResumeState(outputPath string, st *downloadResumeState) error {
	st.UpdatedAt = time.Now().Unix()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := downloadResumePath(outputPath) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, downloadResumePath(outputPath)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeDownloadResumeState 删除 sidecar（成功收尾/远端已变重来时调用）。
func removeDownloadResumeState(outputPath string) {
	_ = os.Remove(downloadResumePath(outputPath))
}

// encodeChunkBitmap 把完成位图编码为 base64（低位在前）。
func encodeChunkBitmap(done []bool) string {
	if len(done) == 0 {
		return ""
	}
	raw := make([]byte, (len(done)+7)/8)
	for i, v := range done {
		if v {
			raw[i/8] |= 1 << uint(i%8)
		}
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeChunkBitmap 解码 base64 位图到长度 total 的布尔切片。
func decodeChunkBitmap(encoded string, total int) []bool {
	done := make([]bool, total)
	if total <= 0 || encoded == "" {
		return done
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return done
	}
	for i := 0; i < total && i/8 < len(raw); i++ {
		if raw[i/8]&(1<<uint(i%8)) != 0 {
			done[i] = true
		}
	}
	return done
}

// downloadResume 是运行期的续传跟踪器（并发安全）：单流记 Offset，分块记位图，
// 并按节流策略落盘 sidecar。内部持有互斥锁，供并发分块下载共享。
type downloadResume struct {
	mu       sync.Mutex
	output   string
	state    downloadResumeState
	done     []bool // 分块模式：已完成位图；单流模式为 nil
	unsaved  int
	lastSave time.Time
}

// newDownloadResume 创建单流续传跟踪器。
func newDownloadResume(outputPath string, st downloadResumeState) *downloadResume {
	st.Version = downloadResumeVersion
	st.ChunkSize = 0
	r := &downloadResume{output: outputPath, state: st}
	// 立即落盘：确保「存在不完整输出文件 ⇒ 必然存在 sidecar」，重入时可判别。
	_ = r.saveLocked(true)
	return r
}

// newChunkedResume 创建分块续传跟踪器（totalChunks 决定位图长度）。
func newChunkedResume(outputPath string, st downloadResumeState, totalChunks int) *downloadResume {
	st.Version = downloadResumeVersion
	r := &downloadResume{
		output: outputPath,
		state:  st,
		done:   decodeChunkBitmap(st.Completed, totalChunks),
	}
	_ = r.saveLocked(true)
	return r
}

// offset 返回单流模式已写入字节数。
func (r *downloadResume) offset() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Offset
}

// addOffset 累加单流已写入字节数并节流落盘。
func (r *downloadResume) addOffset(n int64) {
	if r == nil || n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Offset += n
	r.unsaved++
	if r.unsaved >= downloadResumeChunkFlush || time.Since(r.lastSave) > time.Second {
		_ = r.saveLocked(true)
	}
}

// resetOffset 在服务端不支持 Range（返回 200 全量）时把进度归零。
func (r *downloadResume) resetOffset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Offset = 0
	_ = r.saveLocked(true)
}

// isChunkDone 报告分块 idx 是否已完成。
func (r *downloadResume) isChunkDone(idx int) bool {
	if r == nil || idx < 0 || idx >= len(r.done) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done[idx]
}

// markChunkDone 标记分块 idx 完成并节流落盘。
func (r *downloadResume) markChunkDone(idx int) {
	if r == nil || idx < 0 || idx >= len(r.done) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[idx] {
		return
	}
	r.done[idx] = true
	r.unsaved++
	if r.unsaved >= downloadResumeChunkFlush || time.Since(r.lastSave) > time.Second {
		_ = r.saveLocked(true)
	}
}

// save 立即落盘（force=true 时即使未 dirty 也写）。
func (r *downloadResume) save() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.saveLocked(true)
}

// finish 删除 sidecar（下载成功收尾）。
func (r *downloadResume) finish() {
	if r == nil {
		return
	}
	removeDownloadResumeState(r.output)
}

// saveLocked 落盘当前状态（调用方须持有 r.mu）。
func (r *downloadResume) saveLocked(force bool) error {
	if r.done != nil {
		r.state.Completed = encodeChunkBitmap(r.done)
	}
	r.unsaved = 0
	r.lastSave = time.Now()
	return writeDownloadResumeState(r.output, &r.state)
}

// downloadRangeHeader 构造单流续传的 Range 头（offset<=0 时返回空串）。
func downloadRangeHeader(offset int64) string {
	if offset <= 0 {
		return ""
	}
	return fmt.Sprintf("bytes=%d-", offset)
}
