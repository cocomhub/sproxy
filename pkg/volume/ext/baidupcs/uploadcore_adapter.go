// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// uploadcore_adapter.go —— 把**本包既有能力**适配到自研上传内核（S5 装配）。
//
// 设计：docs/design-baidupcs-upload-core.md。内核只做"编排"（状态机/并发/重试/取消/WAL/续传），
// 协议细节与 checksum 全部复用既有实现，**避免第二份实现**：
//
//	Prober    ← Storage.metaByExactList（列举取 md5；绕开会挂起的 Meta）
//	Rapid     ← 库 pcs.RapidUpload（参数取自 stagedUpload + planRapid；**必须带 blockMD5s**）
//	Slicer    ← 按 uploadBlockSize（4MiB，与 blockList 分块一致）切分
//	Parts     ← 既有 baiduMultiUpload.TmpFile（pcs.UploadTmpFile）
//	Merger    ← 既有 baiduMultiUpload.CreateSuperFile（pcs.UploadCreateSuperFile）
//	Verifier  ← 列举取 md5 与本地 md5 比对（数据操作必须 md5 确认）
//	Repair    ← 既有 libraryAdapter.RapidUpload（秒传刷新，零传输自愈）
//
// 关于 Rapid **不复用** libraryAdapter.RapidUpload：后者只返回"是否命中"（刷新路径语义），
// 内核还需要 **upload_id**（分片上传的前置，库 upload_task_unit.go:281-283）⇒ 两者参数组装不同、
// 语义不同；共用会耦合"刷新"与"首次上传"两条路径。故这里独立实现一次 API 调用（约 10 行），
// 并**引用同一份 stagedUpload 参数**（不重复计算 checksum）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// bcPolicy 返回百度上传策略（与既有 multiupload.go 一致：覆盖 = "overwrite"，否则 "skip"）。
func bcPolicy(overwrite bool) string {
	if overwrite {
		return "overwrite"
	}
	return "skip"
}

// logDebug 输出上传诊断（默认 discard；需排查内核问题时注入 Debug logger）。
func (a *ucAdapter) logDebug(msg string, kv ...any) {
	if a.st == nil || a.st.log == nil {
		return
	}
	a.st.log.Debug("uploadcore adapter: "+msg, kv...)
}

// ucLen64Reader 把任意 io.Reader 包装成 fork 库要求的 `rio.ReaderLen64`。
// `Len()` 语义与库 `fileReadedlen64.Len()` 一致：**剩余未读长度**（rio/file.go:65-72）。
type ucLen64Reader struct {
	r   io.Reader
	rem int64
}

func newUCLen64Reader(r io.Reader, size int64) *ucLen64Reader {
	return &ucLen64Reader{r: r, rem: size}
}

func (r *ucLen64Reader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.rem -= int64(n)
	return n, err
}

// Len 返回剩余未读字节数（库用它设置分片请求的 Content-Length）。
func (r *ucLen64Reader) Len() int64 { return r.rem }

// ucPCS 是 adapter 依赖的**窄接口**（依赖倒置）：
//   - 生产实现 libPCS 直接转发 fork 库调用；
//   - 测试可注入 fake ⇒ **L2 契约（建会话先于上传 / 必须合并 / 不可见容忍）可锁进 CI**，
//     不依赖库内部 HTTP 细节（实测：库的 requester 层会绕过直接替换的 Transport）。
type ucPCS interface {
	// CacheUK 取用户 uk（rapidupload 偏移计算需要；缺 STOKEN 会失败）。
	CacheUK() (int64, error)
	// RapidUpload 真秒传：返回 (returnType, uploadID)。returnType==2 表示秒传命中。
	RapidUpload(remote, policy, uploadID, contentMD5, sliceMD5, dataContent, crc32 string,
		offset, length, totalSize, dataTime int64, blockListMD5 []string) (int, string, error)
	// FakeRapidUpload 建立上传会话（rapidupload 未命中时 **uploadid 的来源**）。
	FakeRapidUpload(remote, policy string, length int64) (string, error)
	// UploadTmpFile 上传一个分片（只写会话临时区，**必须**再 CreateSuperFile 才成正式文件）。
	UploadTmpFile(uploadID, remote string, partSeq int, partOffset int64, r io.Reader, size int64) (string, error)
	// CreateSuperFile 合并分片（不调用则远端永远不会出现文件）。
	CreateSuperFile(uploadID, policy string, size int64, remote string, checksumMap map[int]string) error
}

// libPCS 是 ucPCS 的**生产实现**（转发 fork 库；每方法数行、不含业务逻辑）。
type libPCS struct{ c *Client }

// newLibPCS 构造生产实现（c 为我们的库客户端封装）。
func newLibPCS(c *Client) ucPCS { return &libPCS{c: c} }

func (l *libPCS) CacheUK() (int64, error) {
	uk, pcsErr := l.c.PCS().CacheUK()
	if pcsErr != nil {
		return 0, pcsErr
	}
	return uk, nil
}

func (l *libPCS) RapidUpload(remote, policy, uploadID, contentMD5, sliceMD5, dataContent, crc32 string,
	offset, length, totalSize, dataTime int64, blockListMD5 []string) (int, string, error) {
	pcsError, jsonData := l.c.PCS().RapidUpload(remote, policy, uploadID, contentMD5, sliceMD5,
		dataContent, crc32, offset, length, totalSize, dataTime, blockListMD5)
	if pcsError != nil {
		return 0, "", pcsError
	}
	return jsonData.ReturnType, jsonData.UploadID, nil
}

func (l *libPCS) FakeRapidUpload(remote, policy string, length int64) (string, error) {
	pcsError, jsonData := l.c.PCS().FakeRapidUpload(remote, policy, length)
	if pcsError != nil {
		return "", pcsError
	}
	return jsonData.UploadID, nil
}

func (l *libPCS) UploadTmpFile(uploadID, remote string, partSeq int, partOffset int64,
	r io.Reader, size int64) (string, error) {
	mu := newBaiduMultiUpload(l.c, remote)
	return mu.TmpFile(context.Background(), uploadID, remote, partSeq, partOffset,
		newUCLen64Reader(r, size))
}

func (l *libPCS) CreateSuperFile(uploadID, policy string, size int64, remote string,
	checksumMap map[int]string) error {
	mu := newBaiduMultiUpload(l.c, remote)
	return mu.CreateSuperFile("", policy, uploadID, size, checksumMap)
}

// ucAdapter 实现内核所需的全部依赖接口（一次上传会话一个实例）。
type ucAdapter struct {
	st  *Storage
	pcs ucPCS
	// key 是**相对 root 的 key**（列举类操作必须用它：metaByExactList 内部会拼 s.root）；
	// remote 是**完整远端路径**（库 API 需要完整路径）。混用会导致路径双重拼接（实测踩坑）。
	key       string
	stg       *stagedUpload
	localPath string
	remote    string
	overwrite bool
	// repair 是自愈回调（秒传刷新）；nil ⇒ 内核视为"无自愈能力"（校验失败即报错）。
	repair func(ctx context.Context, remote string, st *stagedUpload) (bool, error)
}

// newUCAdapter 构造适配器（staged 参数复用既有 stagedUpload，不重复算 checksum）。
// pcs 为库客户端（发 rapidupload/分片/合并请求）；repair 为可选自愈（秒传刷新）。
func newUCAdapter(st *Storage, pcs ucPCS, key, localPath, remote string,
	stg *stagedUpload, overwrite bool,
	repair func(ctx context.Context, remote string, st *stagedUpload) (bool, error)) *ucAdapter {
	return &ucAdapter{st: st, pcs: pcs, key: key, stg: stg, localPath: localPath,
		remote: remote, overwrite: overwrite, repair: repair}
}

// ExistingMD5 目标当前内容 md5（"" = 不存在）；用精确列举（Meta 可见性偶发延迟，不可作判据）。
func (a *ucAdapter) ExistingMD5(ctx context.Context, targetPath string) (string, error) {
	meta, ok := a.st.metaByExactList(ctx, a.listKey())
	if !ok || meta == nil {
		return "", nil
	}
	return meta.ETag, nil
}

// Rapid 尝试秒传：返回 (upload_id, 是否已命中)。参数取自 stagedUpload（md5/sliceMD5/crc32/
// blockMD5s/size）+ 随机片段（planRapid，含与库逐位一致的偏移算法）。
func (a *ucAdapter) Rapid(ctx context.Context, req uploadcore.Request) (string, bool, error) {
	uk, ukErr := a.pcs.CacheUK()
	if ukErr != nil {
		return "", false, fmt.Errorf("baidupcs: 获取 uk 失败（需 STOKEN）: %w", ukErr)
	}
	now := time.Now().Unix()
	plan, err := planRapid(a.localPath, uk, now)
	if err != nil {
		return "", false, err
	}
	// 一致性校验：stagedUpload 与 planRapid 的整文件 md5 必须一致（否则说明暂存被改动）。
	if a.stg != nil && a.stg.md5 != "" && plan.ContentMD5 != a.stg.md5 {
		return "", false, fmt.Errorf("baidupcs: 暂存文件 md5 漂移（staged=%s 实测=%s）", a.stg.md5, plan.ContentMD5)
	}
	policy := bcPolicy(a.overwrite)
	returnType, rapidID, rErr := a.pcs.RapidUpload(a.remote, policy, "",
		plan.ContentMD5, plan.SliceMD5, plan.DataContent, plan.CRC32,
		plan.Offset, plan.DataLength, plan.Size, plan.DataTime, a.blockList(plan))
	if rErr != nil {
		return "", false, fmt.Errorf("baidupcs: rapidupload 失败: %w", rErr)
	}
	a.logDebug("rapidupload 响应", "return_type", returnType, "upload_id", rapidID,
		"size", plan.Size, "block_size", plan.BlockSize, "blocks", len(a.blockList(plan)),
		"offset", plan.Offset, "data_len", plan.DataLength)
	if returnType == 2 {
		return rapidID, true, nil // 秒传命中（零传输完成）
	}
	// **uploadid 的真正来源**（2026-11-08 实测修正）：rapidupload 未命中时**不返回 uploadid**
	// （实测响应无 uploadid 字段 ⇒ 直接拿它做分片上传会 `uploadid=` 为空 ⇒ 百度 400）。
	// 与库 CLI 的 `NoRapidUpload` 分支一致：先调 `FakeRapidUpload`（xpan create，rtype=1）
	// 建立上传会话并取回 uploadid（库 upload_task_unit.go 的 prepare 分支同构）。
	if rapidID != "" {
		return rapidID, false, nil
	}
	// **uploadid 的来源**：rapidupload 未命中时不返回 uploadid ⇒ 必须用 fake rapid 建会话。
	fakeID, fErr := a.pcs.FakeRapidUpload(a.remote, policy, plan.Size)
	if fErr != nil {
		return "", false, fmt.Errorf("baidupcs: 建立上传会话失败（fake rapid）: %w", fErr)
	}
	a.logDebug("fake rapidupload 响应", "upload_id", fakeID)
	if fakeID == "" {
		return "", false, fmt.Errorf("baidupcs: 上传会话未返回 uploadid（size=%d）", plan.Size)
	}
	return fakeID, false, nil
}

// blockList 返回 rapidupload 的 block_list 参数：**必须与上传分块一致**
// （本包上传块固定 4MiB ⇒ 用同一 uploadBlockSize 计算的列表），见 rapidcalc.go 的约束说明。
func (a *ucAdapter) blockList(plan *rapidPlan) []string {
	if len(plan.BlocksList) > 0 {
		return plan.BlocksList
	}
	if a.stg != nil {
		return a.stg.blockMD5s
	}
	return nil
}

// Plan 按 uploadBlockSize 切分（Seq 从 0 起）。
func (a *ucAdapter) Plan(_ context.Context, req uploadcore.Request) ([]uploadcore.Part, error) {
	size := req.Size
	if size <= 0 && a.stg != nil {
		size = a.stg.size
	}
	if size <= 0 {
		return nil, fmt.Errorf("baidupcs: 分片计划需要文件大小（size=%d）", size)
	}
	bs := int64(uploadBlockSize)
	parts := make([]uploadcore.Part, 0, (size+bs-1)/bs)
	for off := int64(0); off < size; off += bs {
		n := bs
		if off+n > size {
			n = size - off
		}
		parts = append(parts, uploadcore.Part{Seq: len(parts), Offset: off, Size: n})
	}
	return parts, nil
}

// UploadPart 上传单个分片（复用既有 TmpFile = pcs.UploadTmpFile；同 Seq 重传幂等覆盖）。
func (a *ucAdapter) UploadPart(ctx context.Context, uploadID string, p uploadcore.Part) error {
	f, err := os.Open(a.localPath)
	if err != nil {
		return fmt.Errorf("baidupcs: 打开待上传文件: %w", err)
	}
	defer func() { _ = f.Close() }()
	a.logDebug("上传分片参数", "upload_id", uploadID, "seq", p.Seq, "offset", p.Offset, "size", p.Size)
	if _, err := a.pcs.UploadTmpFile(uploadID, a.remote, p.Seq, p.Offset,
		io.NewSectionReader(f, p.Offset, p.Size), p.Size); err != nil {
		return fmt.Errorf("baidupcs: 上传分片 %d: %w", p.Seq, err)
	}
	return nil
}

// Merge 合并分片（复用既有 CreateSuperFile = pcs.UploadCreateSuperFile）。
func (a *ucAdapter) Merge(_ context.Context, uploadID string, req uploadcore.Request, parts []uploadcore.Part) error {
	size := req.Size
	if size <= 0 && a.stg != nil {
		size = a.stg.size
	}
	checksumMap := make(map[int]string, len(parts))
	for _, p := range parts {
		if a.stg != nil && p.Seq < len(a.stg.blockMD5s) {
			checksumMap[p.Seq] = a.stg.blockMD5s[p.Seq]
		}
	}
	policy := bcPolicy(a.overwrite)
	if err := a.pcs.CreateSuperFile(uploadID, policy, size, a.remote, checksumMap); err != nil {
		return fmt.Errorf("baidupcs: 合并分片: %w", err)
	}
	return nil
}

// listKey 返回列举类操作使用的 key（优先相对 key；未提供时退回完整路径的 basename 推导）。
func (a *ucAdapter) listKey() string {
	if a.key != "" {
		return a.key
	}
	return a.remote
}

// Verify 内核阶段的**轻量**一致性检查。
//
// **职责边界（重要，2026-11-08 真实网盘实测后修正）**：
// 权威校验由 `putAttempt` 的既有 `putCheckAfterUpload` 承担 —— 它有**可见性等待窗口**
// （`statVisibleWait`）+ md5 比对 + 秒传刷新，正是为"新对象列举/元信息可见性有延迟"
// （`docs/external-dependencies.md §7.8`：Meta 偶发 ≥60s 不可见）设计的。
//
// 内核若在此处做**严格**校验（列举未见即失败），会在上传刚完成时**假失败**
// （实测：上传成功但立即列举未见 ⇒ verify 失败；路径正确、upload_id 正确）。
// 因此这里只做"**能看到时**必须一致"的检查：
//   - 列举命中且 md5 不符 ⇒ 报错（真问题，交给内核自愈/上层）；
//   - 列举**未见** ⇒ 返回 nil（延迟可见，交由既有等待逻辑判定，不重复造等待循环）。
func (a *ucAdapter) Verify(ctx context.Context, req uploadcore.Request) error {
	meta, ok := a.st.metaByExactList(ctx, a.listKey())
	if !ok || meta == nil {
		a.logDebug("内核校验：目标暂不可见（延迟可见，交由上层等待复核）", "key", a.listKey())
		return nil
	}
	want := req.MD5
	if want == "" && a.stg != nil {
		want = a.stg.md5
	}
	if want != "" && meta.ETag != "" && meta.ETag != want {
		return fmt.Errorf("baidupcs: md5 不一致（远端 %s != 本地 %s）", meta.ETag, want)
	}
	return nil
}

// Repair 自愈：复用既有秒传刷新（零传输重算远端元信息）。
func (a *ucAdapter) Repair(ctx context.Context, req uploadcore.Request) error {
	if a.repair == nil {
		return fmt.Errorf("baidupcs: 未装配自愈能力（秒传刷新）")
	}
	hit, err := a.repair(ctx, a.remote, a.stg)
	if err != nil {
		return fmt.Errorf("baidupcs: 秒传刷新失败: %w", err)
	}
	if !hit {
		return fmt.Errorf("baidupcs: 秒传刷新未命中（无法自愈，需重传）")
	}
	return nil
}

// 保证编译期契约（内核依赖全部被满足）。
var (
	_ uploadcore.Prober        = (*ucAdapter)(nil)
	_ uploadcore.RapidProvider = (*ucAdapter)(nil)
	_ uploadcore.Slicer        = (*ucAdapter)(nil)
	_ uploadcore.PartUploader  = (*ucAdapter)(nil)
	_ uploadcore.Merger        = (*ucAdapter)(nil)
	_ uploadcore.Verifier      = (*ucAdapter)(nil)
	_ uploadcore.Repairer      = (*ucAdapter)(nil)
)
