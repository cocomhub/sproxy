// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_export.go 实现卷备份/导出（roadmap 11.3-② / docs/designs/2026-09-24-volume-export.md）：
//
//   - GET /api/volumes/export?volume=<name> → `application/x-tar` 流式响应：逐文件写
//     `user/<rel>` 条目（流式边读边写，不整卷入内存），末尾 append `manifest.json`
//     （每条目相对路径 + SHA-256 + size + mtime + 台账 checksum 交叉校验）；
//   - POST /api/volumes/import?volume=<name>[&overwrite=true]（body = tar 流）→ 逐条目
//     解包 → 复用既有写路径（ValidateFilePath 校验 + 配额 Reserve + 写后 checksum 登记
//     ——不 bypass 台账），manifest 校验每条目 checksum 一致才落盘（不一致 → 该条目标记
//     失败并跳过，报告列出）。
//
// 权限：导出 = 读卷（fileRouteRead）；导入 = 写卷 + 配额（fileRoute）。导入覆盖需显式
// `overwrite=true`（同名已存在且未显式覆盖 → 跳过 + 报告，防误覆盖）。
//
// 错误处理（设计文档）：
//   - 导出中卷文件被修改/删除 → 该条目记 warning 继续（宽松快照语义；strict=true 才中止）；
//   - 导入 tar 损坏/条目路径穿越（ValidateFilePath 拒绝）→ 该条目跳过 + 报告（绝不写卷外）；
//   - 配额不足 → 该条目跳过 + 报告（不半卷提交）；
//   - 导入半途失败 → 已写入条目保留（可重跑，幂等：同名覆盖需显式 overwrite）。

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// exportManifestEntry 是导出 tar 尾部 manifest.json 的单个条目。
type exportManifestEntry struct {
	Path     string `json:"path"`             // 租户根相对路径（user/<rel>）
	Size     int64  `json:"size"`             // 文件大小
	MTime    int64  `json:"mtime"`            // 修改时间（UnixNano）
	Checksum string `json:"checksum"`         // 实时重算的 SHA-256
	Ledger   string `json:"ledger,omitempty"` // 台账登记的 SHA-256（交叉校验；空 = 台账无）
}

// exportManifest 是导出 tar 尾部的清单结构。
type exportManifest struct {
	Volume string                `json:"volume"` // 源卷名（空 = 无卷语义）
	Files  []exportManifestEntry `json:"files"`
	Warn   []string              `json:"warnings,omitempty"` // 导出中源文件被修改/删除的 warning
}

// importResult 是 POST /api/volumes/import 的响应体。
type importResult struct {
	Success  bool     `json:"success"`
	Message  string   `json:"message"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"` // 已存在未覆盖 / 配额不足 / 路径非法 / checksum 不符
	Failed   int      `json:"failed"`  // 单条写盘失败
	Errors   []string `json:"errors,omitempty"`
}

// volumeExportSource 是导出枚举出的单文件（用户相对路径 → 卷内位置）。
type volumeExportSource struct {
	relName string // 用户相对路径（不含 user/ 前缀，tar 条目 = "user/"+relName）
	size    int64
	mtime   time.Time
	root    *storage.Root // 实际所在卷租户根
	rel     string        // 租户根相对路径（user/<relName>）
}

// exportVolumeHandler 处理 GET /api/volumes/export?volume=<name>[&strict=true]。
//
// 流式语义：枚举 owner 卷视图内（或指定卷）user 桶全部文件（递归）→ 逐文件 tar 写入
// （边读边写，不整卷入内存）→ 写完 append manifest.json → 200 `application/x-tar`。
// 导出中单文件被并发修改/删除 → 该条目记 warning（宽松）或 400 中止（strict=true）。
func (h *Handlers) exportVolumeHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	explicitVol := r.URL.Query().Get("volume")
	strict := r.URL.Query().Get("strict") == "true"

	sources, err := h.listVolumeExportSources(owner, explicitVol)
	if err != nil {
		h.logger.Error("导出卷：枚举失败", "volume", explicitVol, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取卷文件失败"}, http.StatusInternalServerError)
		return
	}

	// 目标卷名（manifest 标注；显式卷 = 该卷名；自动视图 = 默认卷名）。
	volName := h.resolveExportVolName(explicitVol)

	w.Header().Set(headerContentType, "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"volume-%s-%d.tar\"", safeManifestName(volName), time.Now().Unix()))
	w.WriteHeader(http.StatusOK)

	tw := tar.NewWriter(w)
	warnings, werr := h.writeExportEntries(r.Context(), tw, sources, strict)
	if werr != nil {
		_ = tw.Close()
		return
	}

	// 尾部 manifest.json（每一条目相对路径 + SHA-256 + size + mtime + 台账交叉校验）。
	mf, merr := h.buildExportManifest(owner, sources, volName, strict)
	if merr != nil {
		_ = tw.Close()
		return
	}
	mf.Warn = warnings
	writeExportManifest(tw, mf)
	_ = tw.Close()

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "volume_export", ObjectType: "volume", Object: volName,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("files=%d strict=%v", len(mf.Files), strict),
	})
}

// resolveExportVolName 解析导出目标卷名（显式卷 = 该卷名；空 = 默认卷名）。
func (h *Handlers) resolveExportVolName(explicitVol string) string {
	volName := explicitVol
	if volName == "" && h.volSet != nil {
		volName = h.volSet.Default().Name
	}
	return volName
}

// writeExportEntries 逐文件写入 tar 条目（strict=false 时失败记 warning 继续；
// strict=true 时首个条目失败即中止并返回错误）。
func (h *Handlers) writeExportEntries(ctx context.Context, tw *tar.Writer, sources []volumeExportSource, strict bool) ([]string, error) {
	var warnings []string
	for _, s := range sources {
		if err := h.writeVolumeExportEntry(ctx, tw, s); err != nil {
			if strict {
				h.logger.Warn("导出卷：strict 中止", "file", s.relName, "error", err)
				return warnings, err
			}
			warnings = append(warnings, fmt.Sprintf("%s: %v", s.relName, err))
			continue
		}
	}
	return warnings, nil
}

// buildExportManifest 构造导出 manifest（含台账交叉校验）。strict=true 且单文件读失败时
// 返回错误（调用方中止导出）。
func (h *Handlers) buildExportManifest(owner string, sources []volumeExportSource, volName string, strict bool) (exportManifest, error) {
	mf := exportManifest{Volume: volName}
	ledger := h.checksumStoreFor(owner)
	for _, s := range sources {
		cs, cerr := FileChecksumRoot(s.root, s.rel)
		if cerr != nil {
			if strict {
				return mf, cerr
			}
			continue // 单文件读失败（并发删）：manifest 缺该条目（宽松）
		}
		entry := exportManifestEntry{
			Path:     filepath.ToSlash(s.rel),
			Size:     s.size,
			MTime:    s.mtime.UnixNano(),
			Checksum: cs,
		}
		if ledger != nil {
			if v, ok := ledger.Get(filepath.ToSlash(s.rel)); ok {
				entry.Ledger = v
			}
		}
		mf.Files = append(mf.Files, entry)
	}
	return mf, nil
}

// writeExportManifest 把 manifest.json 条目写入 tar 末尾（序列化失败时静默跳过）。
func writeExportManifest(tw *tar.Writer, mf exportManifest) {
	raw, merr := json.MarshalIndent(mf, "", "  ")
	if merr != nil {
		return
	}
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(raw))})
	_, _ = tw.Write(raw)
}

// safeManifestName 把卷名归一为 Content-Disposition 可安全使用的文件名（空 → "default"）。
func safeManifestName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(name)
}

// listVolumeExportSources 枚举 owner 卷视图内（或指定卷）user 桶全部文件（递归）。
// 返回按 relName 排序的源列表。仅本地卷（storage.Root 非 nil）可导出；外部卷无本地根 → 错误。
func (h *Handlers) listVolumeExportSources(owner, explicitVol string) ([]volumeExportSource, error) {
	switch {
	case explicitVol != "":
		if h.volSet == nil {
			return nil, fmt.Errorf("卷功能未装配")
		}
		v, ok := h.volSet.ByName(explicitVol)
		if !ok || !v.Authorize(owner) {
			return nil, errors.New(msgVolumeNotAllowed)
		}
		return h.walkExportVolumes([]volume.Volume{v}, owner)
	case h.volSet != nil:
		return h.walkExportVolumes(volume.AllowedVolumes(h.volSet.All(), owner), owner)
	default:
		// 无卷语义（旧装配路径）：唯一根 = 默认租户。
		tnt := h.tenantFor(owner)
		if tnt == nil || tnt.Root() == nil {
			return nil, fmt.Errorf("租户不可用")
		}
		return h.walkVolumeUserFiles("", tnt.Root(), owner)
	}
}

// walkExportVolumes 逐个卷枚举 user 桶文件（外部卷无本地根则跳过），合并后按 relName 排序。
func (h *Handlers) walkExportVolumes(vols []volume.Volume, owner string) ([]volumeExportSource, error) {
	if len(vols) == 0 {
		return nil, nil
	}
	var out []volumeExportSource
	for _, v := range vols {
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue // 外部卷无本地根：跳过（不导）
		}
		srcs, err := h.walkVolumeUserFiles(v.Name, tnt.Root(), owner)
		if err != nil {
			return nil, err
		}
		out = append(out, srcs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].relName < out[j].relName })
	return out, nil
}

// volumeUserWalker 遍历单个卷租户 user 桶目录（递归），收集 volumeExportSource 列表。
type volumeUserWalker struct {
	root *storage.Root
	out  []volumeExportSource
}

// walkVolumeUserFiles 递归列出卷租户 user 桶下全部文件。
func (h *Handlers) walkVolumeUserFiles(volName string, root *storage.Root, owner string) ([]volumeExportSource, error) {
	w := &volumeUserWalker{root: root}
	if err := w.walk("user", 0); err != nil {
		return nil, err
	}
	return w.out, nil
}

// walk 递归遍历 user 桶目录（深度上界防符号链接环），把文件条目收集到 out。
func (w *volumeUserWalker) walk(rel string, depth int) error {
	if depth > 100 {
		return fmt.Errorf("目录深度超过限制: %s", rel)
	}
	entries, err := w.root.ReadDir(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		childRel := filepath.ToSlash(filepath.Join(rel, e.Name()))
		if e.IsDir() {
			if err := w.walk(childRel, depth+1); err != nil {
				return err
			}
			continue
		}
		w.addFile(childRel, e)
	}
	return nil
}

// addFile 把单个文件条目加入 out（stat 失败/非 user/ 前缀则忽略）。
func (w *volumeUserWalker) addFile(childRel string, e os.DirEntry) {
	info, ierr := e.Info()
	if ierr != nil {
		return // 单条目 stat 失败跳过（尽力而为）
	}
	relName, ok := strings.CutPrefix(childRel, "user/")
	if !ok {
		return
	}
	w.out = append(w.out, volumeExportSource{relName: relName, size: info.Size(), mtime: info.ModTime(), root: w.root, rel: childRel})
}

// writeVolumeExportEntry 把单个文件以 user/<relName> 条目写入 tar（流式边读边写）。
// 源在写前被并发删除/修改导致 Open 失败 → 返回错误（调用方按宽松/严格处置）。
func (h *Handlers) writeVolumeExportEntry(ctx context.Context, tw *tar.Writer, s volumeExportSource) error {
	f, err := s.root.Open(s.rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, serr := f.Stat()
	if serr != nil {
		return serr
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    "user/" + filepath.ToSlash(s.relName),
		Mode:    0o644,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}); err != nil {
		return err
	}
	if _, err := copyWithContext(tw, f, ctx); err != nil {
		return err
	}
	return nil
}

// importVolumeHandler 处理 POST /api/volumes/import?volume=<name>[&overwrite=true]。
//
// 数据流：解 tar → 先读尾部 manifest.json（存在时先校验每条目 checksum）→ 逐文件条目
// 解包 → ValidateFilePath 校验（拒绝 .. / 绝对路径）→ 配额 Reserve（不足 → 跳过 + 报告）
// → 同名已存在且未显式 overwrite → 跳过 + 报告 → 写盘（复用既有写路径 WriteFile：
// 原子写 + checksum 门禁 + 台账登记，不 bypass 台账）→ 落盘后重算 checksum 与 manifest
// 比对，不符 → 该条目标记失败并跳过。
func (h *Handlers) importVolumeHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	explicitVol := r.URL.Query().Get("volume")
	overwrite := r.URL.Query().Get("overwrite") == "true"

	res := importResult{Success: true, Message: "导入完成"}

	// 先扫一遍收集 manifest.json（tar 条目可能乱序；manifest 通常尾部）。
	manifestRaw, pending, scanErrs := collectImportEntries(tar.NewReader(r.Body))
	res.Failed += len(scanErrs)
	res.Errors = append(res.Errors, scanErrs...)

	// 解析 manifest（缺失 = 无清单，仍可恢复内容；有清单则校验）。
	manifest, haveManifest, merr := parseImportManifest(manifestRaw)
	if merr != nil {
		res.Errors = append(res.Errors, merr.Error())
	}
	manifestCS, manifestSize := manifestIndex(manifest)

	for _, p := range pending {
		ic := &importEntryCtx{
			r: r, owner: owner, explicitVol: explicitVol, overwrite: overwrite,
			name: p.name, content: p.content,
			haveManifest: haveManifest, manifestCS: manifestCS, manifestSize: manifestSize,
			res: &res,
		}
		if err := h.importOneFile(ic); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p.name, err))
		}
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "volume_import", ObjectType: "volume", Object: explicitVol,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("imported=%d skipped=%d failed=%d overwrite=%v", res.Imported, res.Skipped, res.Failed, overwrite),
	})
	sendJSONResponse(w, res, http.StatusOK)
}

// importPendingEntry 是导入扫描阶段暂存的单个 tar 文件条目。
type importPendingEntry struct {
	name    string
	content []byte
}

// collectImportEntries 先扫一遍 tar 流：收集 manifest.json 原始字节与全部普通文件条目到
// pending。返回 (manifestRaw, pending, 扫描阶段错误列表)；tar 损坏即中止扫描（返回已收集部分）。
func collectImportEntries(tr *tar.Reader) ([]byte, []importPendingEntry, []string) {
	var manifestRaw []byte
	var pending []importPendingEntry
	var scanErrs []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			scanErrs = append(scanErrs, fmt.Sprintf("tar 损坏: %v", err))
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // 目录/符号链接等非普通文件条目忽略
		}
		data, rerr := io.ReadAll(io.LimitReader(tr, 1<<30)) // 单文件上限 1 GiB（超大走分块上传）
		if rerr != nil {
			scanErrs = append(scanErrs, fmt.Sprintf("读条目 %s 失败: %v", hdr.Name, rerr))
			continue
		}
		if hdr.Name == "manifest.json" {
			manifestRaw = data
			continue
		}
		pending = append(pending, importPendingEntry{name: hdr.Name, content: data})
	}
	return manifestRaw, pending, scanErrs
}

// parseImportManifest 解析 manifest 字节流；无字节 = 无清单（返回 haveManifest=false）。
func parseImportManifest(raw []byte) (exportManifest, bool, error) {
	var manifest exportManifest
	if len(raw) == 0 {
		return manifest, false, nil
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, false, fmt.Errorf("manifest 解析失败: %v", err)
	}
	return manifest, true, nil
}

// manifestIndex 构建按路径索引的清单 checksum/size 映射（供条目复核）。
func manifestIndex(manifest exportManifest) (map[string]string, map[string]int64) {
	manifestCS := map[string]string{}
	manifestSize := map[string]int64{}
	for _, f := range manifest.Files {
		manifestCS[f.Path] = f.Checksum
		manifestSize[f.Path] = f.Size
	}
	return manifestCS, manifestSize
}

// importEntryCtx 是导入单个 tar 条目的完整上下文（由 importVolumeHandler 每个 pending 构造，
// importOneFile 消费；写盘失败收尾经 importWriteCtx 交 resolveImportWriteError/retryImportOverwrite）。
type importEntryCtx struct {
	r            *http.Request
	owner        string
	explicitVol  string
	overwrite    bool
	name         string // tar 条目名（user/<rel>）
	content      []byte
	haveManifest bool
	manifestCS   map[string]string
	manifestSize map[string]int64
	res          *importResult
}

// importWriteCtx 承载导入条目写盘与覆盖收尾共享的上下文（resolveImportWriteError /
// retryImportOverwrite 复用；probeVol = 探测/删除卷名，显式卷；空 → 默认卷）。
type importWriteCtx struct {
	r           *http.Request
	name        string // tar 条目名（user/<rel>）
	owner       string
	probeVol    string
	explicitVol string
	userRel     string // 写盘用户相对路径（不含 user/ 前缀）
	content     []byte
	overwrite   bool
	exists      bool
	res         *importResult
}

// importOneFile 导入单个 tar 条目（校验 + 配额 + 写盘 + manifest 复核）。
// 失败返回错误；跳过（已存在/配额不足/路径非法/checksum 不符）记入 ic.res.Skipped 并返回 nil。
func (h *Handlers) importOneFile(ic *importEntryCtx) error {
	// 路径校验（ValidateFilePath 拒绝 .. / 绝对路径 / 空字节 / Windows 非法字符）。
	// ic.name 是 tar 条目名（"user/<rel>"，export 侧写入格式）；校验与 manifest 比对用完整
	// 条目名，**写盘路径**剥离 user/ 前缀传用户相对路径（files.WriteFile 的
	// resolveWritePath 会再做 UserRel 映射——传含 user/ 的路径会落到 user/user/ 子目录
	// 且覆盖探测恒 miss）。
	if _, err := pathguard.ValidateFilePath(ic.name); err != nil {
		ic.res.Skipped++
		ic.res.Errors = append(ic.res.Errors, fmt.Sprintf("%s: 路径非法（拒绝写入卷外）: %v", ic.name, err))
		return nil
	}
	userRel, _ := strings.CutPrefix(filepath.ToSlash(ic.name), "user/")

	// manifest 校验：条目 checksum 与清单不一致 → 该条目标记失败并跳过（设计文档）。
	if mismatch, merr := manifestMismatch(ic.name, ic.content, ic.haveManifest, ic.manifestCS); merr != nil {
		return merr
	} else if mismatch {
		ic.res.Skipped++
		ic.res.Errors = append(ic.res.Errors, fmt.Sprintf("%s: checksum 与 manifest 不符（跳过）", ic.name))
		return nil
	}

	// 复用既有写路径（files.WriteFile：原子写 + checksum 门禁 + 台账登记，不 bypass 台账）。
	// 写前先探测同名已存在：未显式 overwrite → 跳过 + 报告（幂等防误覆盖）。
	// checksum 用 pkg/checksum.Reader（单一事实源）。
	// 探测卷名：显式卷用显式名；空（未指定）→ 使用默认卷名（volSet.Root("") 为 nil 会
	// 导致 volumeFileExists 恒 miss → 覆盖保护失效，故必须先归一，语义与
	// exportVolumeHandler 的 volName 解析一致）。
	probeVol := ic.explicitVol
	if probeVol == "" && h.volSet != nil {
		probeVol = h.volSet.Default().Name
	}
	contentCS, cerr := checksum.Reader(bytes.NewReader(ic.content))
	if cerr != nil {
		return cerr
	}
	exists, err := h.volumeFileExists(probeVol, ic.owner, "user/"+userRel)
	if err != nil {
		return err
	}
	if exists && !ic.overwrite {
		ic.res.Skipped++
		ic.res.Errors = append(ic.res.Errors, fmt.Sprintf("%s: 已存在（需 --overwrite 显式覆盖）", ic.name))
		return nil
	}

	wc := &importWriteCtx{
		r: ic.r, name: ic.name, owner: ic.owner, probeVol: probeVol,
		explicitVol: ic.explicitVol, userRel: userRel, content: ic.content,
		overwrite: ic.overwrite, exists: exists, res: ic.res,
	}
	input := files.WriteFileInput{
		Owner:            ic.owner,
		RemotePath:       userRel,
		ExplicitVol:      ic.explicitVol,
		ExpectedChecksum: contentCS,
		ClientSize:       int64(len(ic.content)),
	}
	if _, werr := h.fileService().WriteFile(ic.r.Context(), input, bytes.NewReader(ic.content)); werr != nil {
		return h.resolveImportWriteError(wc, input, werr)
	}
	ic.res.Imported++
	return nil
}

// manifestMismatch 校验条目 checksum 与清单是否一致（无清单/无该条目/空期望 = 通过）。
func manifestMismatch(name string, content []byte, haveManifest bool, manifestCS map[string]string) (bool, error) {
	if !haveManifest {
		return false, nil
	}
	wantCS, ok := manifestCS[filepath.ToSlash(name)]
	if !ok || wantCS == "" {
		return false, nil
	}
	gotCS, gerr := checksum.Reader(bytes.NewReader(content))
	if gerr != nil {
		return false, gerr
	}
	return gotCS != wantCS, nil
}

// resolveImportWriteError 处理 WriteFile 首次失败的收尾：overwrite 冲突 → 删旧重建；
// 配额不足 → 跳过；其余直返。
func (h *Handlers) resolveImportWriteError(wc *importWriteCtx, input files.WriteFileInput, werr error) error {
	if wc.overwrite && wc.exists {
		if handled, herr := h.retryImportOverwrite(wc, input, werr); handled {
			return herr
		}
	}
	if isQuotaError(werr) {
		return importQuotaSkip(wc.name, wc.res)
	}
	return werr
}

// retryImportOverwrite 处理 overwrite 语义的 409 冲突：先删旧再重建（覆盖语义 = 删除 +
// 重建，配额差分由 routeUpload 重新 Reserve）。返回 handled=true 表示本次导入已收尾。
func (h *Handlers) retryImportOverwrite(wc *importWriteCtx, input files.WriteFileInput, werr error) (bool, error) {
	if !isConflictError(werr) {
		return false, nil
	}
	// 覆盖写（overwrite=true）时同名旧文件 checksum 不同：WriteFile 的
	// handleDuplicateFile（自动路由）会以 409 冲突拒绝（versioning 关闭）——
	// 导入的 overwrite 语义是「允许覆盖」，此处将 409 冲突视为可覆盖条件。
	// 安全：删除只作用于卷内已探测存在的同 rel（绝不触碰其它路径）。
	if rt := h.volumeTenant(wc.probeVol, wc.owner); rt != nil && rt.Root() != nil {
		if rerr := rt.Root().Remove("user/" + wc.userRel); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return true, rerr
		}
	}
	input.ExplicitVol = wc.explicitVol
	if _, werr2 := h.fileService().WriteFile(wc.r.Context(), input, bytes.NewReader(wc.content)); werr2 != nil {
		if isQuotaError(werr2) {
			return true, importQuotaSkip(wc.name, wc.res)
		}
		return true, werr2
	}
	wc.res.Imported++
	return true, nil
}

// importQuotaSkip 记录一次配额不足跳过并返回 nil（表示已收尾，调用方直接返回）。
func importQuotaSkip(name string, res *importResult) error {
	res.Skipped++
	res.Errors = append(res.Errors, fmt.Sprintf("%s: 配额不足（跳过）", name))
	return nil
}

// isConflictError 判定 WriteFile 失败是否属同名冲突（409）。
func isConflictError(err error) bool {
	if he, ok := errors.AsType[*files.HTTPError](err); ok {
		return he.Status == http.StatusConflict
	}
	return false
}

// isQuotaError 判定 WriteFile 失败是否属配额不足（HTTP 507）。
func isQuotaError(err error) bool {
	if he, ok := errors.AsType[*files.HTTPError](err); ok {
		return he.Status == http.StatusInsufficientStorage
	}
	return false
}
