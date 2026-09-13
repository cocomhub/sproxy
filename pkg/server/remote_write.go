// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_write.go 是 B 侧跨节点**写面**（Y 二期 P3-b；规格 §5.7/5.8/5.9）。
//
// 定位与只读面**反向同构**：
//   - 只读面物理上只注册 GET/HEAD（AD-7 非黑名单法）；本写面物理上只注册 4 条 POST 写 op，
//     只读路径与本地路径都不在这张表上；
//   - 授权同样「指纹反查 + 三重约束」，但写必须 `AuthorizeMeshWrite`（scope 授予写）；
//   - **只做授权 + 调域 API**：checksum 门禁、原子改名、版本、配额、文件锁、卷路由全部由
//     `pkg/files` 的域方法承担——本文件绝不复制写语义（规格 §5.9 的根治目标）。
//
// 与本地写的关系（§5.8）：远程写与本地写**共享同一锁池**（`pkg/files` 的 FileLocks），
// 故跨节点互斥天然成立；本层不引入分布式锁/版本向量/冲突判定（冲突判定留在上层 `pkg/sync`）。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/files"
)

// remoteWriteResponse 是写面的成功响应体（消费者是 A 侧 `pkg/remote`）。
//
// 失败一律用纯文本（`writeRemoteError`，只按状态码给通用文案）——与只读面同一原则：
// 拒绝响应不泄露卷/文件存在性，客户端据**状态码**分派（409 冲突 / 404 不存在 / 400 参数或
// checksum 不符）。
type remoteWriteResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Volume   string `json:"volume,omitempty"`
}

// remoteWriteHandler 是跨节点写面的路由表与授权器（每连接一个：指纹是连接级属性）。
type remoteWriteHandler struct {
	h    *Handlers
	peer peerFingerprintProvider
}

// newRemoteWriteHandler 构造写路由表——**独立白名单**：只有这 4 条 POST。
//
// 读路径（/remote/list|stat|download）与本地路径（/upload…）都不注册 ⇒ 访问它们得到 404
// 而不是 405/403：写面不是「读面 + 黑名单」，而是另一张白名单，两者物理隔离。
func (h *Handlers) newRemoteWriteHandler(peer peerFingerprintProvider) http.Handler {
	wh := &remoteWriteHandler{h: h, peer: peer}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /remote/write", wh.handleWrite)
	mux.HandleFunc("POST /remote/rename", wh.handleRename)
	mux.HandleFunc("POST /remote/delete", wh.handleDelete)
	mux.HandleFunc("POST /remote/mkdir", wh.handleMkdir)
	return mux
}

func (wh *remoteWriteHandler) handleWrite(w http.ResponseWriter, r *http.Request) {
	wh.serve(w, r, "write")
}

func (wh *remoteWriteHandler) handleRename(w http.ResponseWriter, r *http.Request) {
	wh.serve(w, r, "rename")
}

func (wh *remoteWriteHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	wh.serve(w, r, "delete")
}

func (wh *remoteWriteHandler) handleMkdir(w http.ResponseWriter, r *http.Request) {
	wh.serve(w, r, "mkdir")
}

// serve 统一走「授权 → 委派域方法 → 审计」三步（与只读面同构）；拒绝路径在 authorize 内记审计。
func (wh *remoteWriteHandler) serve(w http.ResponseWriter, r *http.Request, op string) {
	tgt, to, ok := wh.authorize(w, r, op)
	if !ok {
		return
	}
	sw := &remoteStatusWriter{ResponseWriter: w}
	wh.delegate(sw, r, tgt, to, op)
	wh.h.RecordAudit(r.Context(), AuditEvent{
		Action: "mesh_write", Actor: tgt.owner, Mesh: tgt.node,
		ObjectType: "file", Object: tgt.path,
		Result: remoteAuditResult(sw.status),
		Detail: "volume=" + tgt.vol.Name + " path=" + tgt.path + " op=" + op +
			" status=" + strconv.Itoa(sw.status),
	})
}

// authorize 解析并校验写授权；不通过时写错误响应并记审计，返回 ok=false。
//
// 与只读面**同一套三步**（缺一不可）：
//   - `MeshReaderFor(fp)` 只做指纹反查，取回 (node, owner) 绑定——**owner 由此而来**，
//     绝不由请求参数决定（防越权枚举的红线）；
//   - `AuthorizeMeshWrite(node, fp, owner)` 施加第二、三重约束：三元组命中 + 条目 **scope
//     授予写**（read 不隐含写）+ owner 过本卷 ACL。
//
// op == "rename" 时同时解析目标路径（`to`）并返回；其余 op 的 `to` 为空。
func (wh *remoteWriteHandler) authorize(w http.ResponseWriter, r *http.Request, op string) (*remoteTarget, string, bool) {
	q := r.URL.Query()
	volName := strings.TrimSpace(q.Get("volume"))
	// 路径归一与只读面一致：容忍前导 `/`（远端绝对写法），`..` 等穿越仍由域方法的
	// pathguard 拒绝（本层不自行实现路径校验——那正是「复制语义」的开端）。
	relPath := normalizeRemotePath(q.Get("path"))
	to := ""
	if op == "rename" {
		// rename 的源在 `from`（与本地 /rename 参数名一致）；`path` 不参与。
		relPath = normalizeRemotePath(q.Get("from"))
		to = normalizeRemotePath(q.Get("to"))
	}

	// deny 记审计 + 写错误响应。状态语义与只读面一致：授权类拒绝一律 404（不泄露卷/文件
	// 存在性），未认证 401，服务端装配错误 500（冒充 404 会误导排障）。
	deny := func(status int, result, detail string) (*remoteTarget, string, bool) {
		wh.h.RecordAudit(r.Context(), AuditEvent{
			Action: "mesh_write", ObjectType: "file", Object: relPath,
			Result: result, Detail: "volume=" + volName + " path=" + relPath + " op=" + op + ": " + detail,
		})
		writeRemoteError(w, status, remoteErrorMessage(status))
		return nil, "", false
	}

	fp := ""
	if wh.peer != nil {
		fp = strings.TrimSpace(wh.peer.PeerFingerprint())
	}
	if fp == "" {
		return deny(http.StatusUnauthorized, AuditResultDenied, "对端无已认证身份指纹")
	}
	if volName == "" {
		return deny(http.StatusNotFound, AuditResultDenied, "缺少 volume 参数")
	}
	if wh.h.volSet == nil {
		return deny(http.StatusInternalServerError, AuditResultError, "卷集合未装配")
	}
	vol, ok := wh.h.volSet.ByName(volName)
	if !ok {
		return deny(http.StatusNotFound, AuditResultDenied, "卷不存在")
	}
	mr, ok := vol.MeshReaderFor(fp)
	if !ok {
		return deny(http.StatusNotFound, AuditResultDenied, "指纹未列入本卷 mesh_readers")
	}
	if !vol.AuthorizeMeshWrite(mr.Node, fp, mr.Owner) {
		return deny(http.StatusNotFound, AuditResultDenied,
			"写授权未通过 node="+mr.Node+" owner="+mr.Owner+"（scope 须授予 write）")
	}
	return &remoteTarget{vol: vol, node: mr.Node, owner: mr.Owner, path: relPath}, to, true
}

// delegate 在**已完成授权**的前提下执行写操作：**直调 pkg/files 域方法**，自行写响应。
//
// 每个 op 只做「把 HTTP 入参翻成域入参 → 调域方法 → 把域结果翻成响应」，不实现任何写语义。
// `ExplicitVol` 恒为已授权卷：域侧的卷路由/ACL 复核与唯一性检查（AD-4）对远程写同样生效，
// 使远程写与本地写在**同一份约束**下收敛。
func (wh *remoteWriteHandler) delegate(w http.ResponseWriter, r *http.Request, tgt *remoteTarget, to, op string) {
	svc := wh.h.fileService()
	checksum := strings.TrimSpace(r.Header.Get(headerFileChecksum))

	switch op {
	case "write":
		// 与本地 multipart 上传**同一硬上限**（单一事实源 internal/size.UploadBodyLimit）：
		// 远程面不得成为绕过上限的旁路——否则一个被授权的节点就能写满磁盘。
		body := http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
		res, err := svc.WriteFile(r.Context(), files.WriteFileInput{
			Owner:            tgt.owner,
			RemotePath:       tgt.path,
			ExplicitVol:      tgt.vol.Name,
			ExpectedChecksum: checksum,
			ClientSize:       r.ContentLength,
			Mtime:            remoteMTimeNano(r),
		}, body)
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, remoteWriteResponse{
			Success: true, Message: res.Message, Checksum: res.Checksum,
			Size: res.Size, Volume: res.VolumeName,
		}, http.StatusOK)

	case "rename":
		res, err := svc.RenameFile(r.Context(), files.RenameFileInput{
			Owner:            tgt.owner,
			From:             tgt.path,
			To:               to,
			ExpectedChecksum: checksum,
			ExplicitVol:      tgt.vol.Name,
		})
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, remoteWriteResponse{
			Success: true, Message: res.Message, Checksum: res.Checksum, Volume: tgt.vol.Name,
		}, http.StatusOK)

	case "delete":
		res, err := svc.DeleteFile(r.Context(), files.DeleteFileInput{
			Owner:            tgt.owner,
			RemotePath:       tgt.path,
			ExpectedChecksum: checksum,
			ExplicitVol:      tgt.vol.Name,
		})
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, remoteWriteResponse{
			Success: true, Message: res.Message, Volume: tgt.vol.Name,
		}, http.StatusOK)

	case "mkdir":
		res, err := svc.MakeDir(tgt.owner, tgt.path)
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, remoteWriteResponse{
			Success: true, Message: "目录已创建: " + res.RemotePath, Volume: tgt.vol.Name,
		}, http.StatusOK)

	default:
		writeRemoteError(w, http.StatusNotFound, remoteErrorMessage(http.StatusNotFound))
	}
}

// normalizeRemotePath 归一远端传来的路径：去首尾空白 + 剥前导 `/`（远端「绝对写法」= owner
// user 桶内相对路径，与 sclient `cd /` 的根语义一致）。
//
// 用 TrimLeft 而非 TrimPrefix：`//docs` 与 `/docs` 必须等价。**只做归一，不做校验**——路径
// 合法性（`..`、绝对路径、内部前缀段）由域方法的 pathguard 判定，避免这里长出第二份规则。
func normalizeRemotePath(p string) string {
	return strings.TrimLeft(strings.TrimSpace(p), "/")
}

// remoteMTimeNano 解析 `X-File-MTime`（UnixNano）；缺失/非法/非正数返回 0（= 不设置时间戳）。
// 语义与本地上传的 X-File-MTime 完全一致（域侧 `WriteFileInput.Mtime` 同约定）。
func remoteMTimeNano(r *http.Request) int64 {
	v := strings.TrimSpace(r.Header.Get(headerFileMTime))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
