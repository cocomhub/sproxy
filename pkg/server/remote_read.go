// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// peerFingerprintProvider 抽象「已认证对端指纹」的来源。
//
// 生产实现是 *tunnel.Tunnel：Serve 在进入 accept 循环前完成双向 Ed25519 pin 握手，
// PeerFingerprint() 返回握手获得的对端指纹。测试注入伪造实现以驱动授权矩阵。
type peerFingerprintProvider interface {
	PeerFingerprint() string
}

// remoteReadHandler 是跨节点只读面（Y 一期，AD-7/AD-8）。
//
// 只读强制：路由表是手写白名单，仅注册三条 GET/HEAD；写方法由 http.ServeMux 的
// 方法模式天然 405，写 handler 从不出现在这张表上（非黑名单法）。
//
// 授权：owner 恒由配置（mesh_readers 条目）决定，绝不接受请求方指定；卷名必须命中
// 本节点已配置的卷，且该卷 mesh_readers 中存在指纹命中的条目。
type remoteReadHandler struct {
	h    *Handlers
	peer peerFingerprintProvider
}

// remoteTarget 是一次已授权远程读的目标上下文。
type remoteTarget struct {
	vol   volume.Volume
	node  string // 审计用：对端 mesh 节点 ID（来自命中的 mesh_readers 条目）
	owner string // 受限 context 中注入的 owner（来自命中的 mesh_readers 条目）
	path  string // owner user 桶内相对路径
}

// newRemoteReadHandler 构造只读路由表（每连接一个：指纹是连接级属性）。
func (h *Handlers) newRemoteReadHandler(peer peerFingerprintProvider) http.Handler {
	rh := &remoteReadHandler{h: h, peer: peer}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote/list", rh.handleList)
	mux.HandleFunc("HEAD /remote/stat", rh.handleStat)
	mux.HandleFunc("GET /remote/download", rh.handleDownload)
	return mux
}

func (rh *remoteReadHandler) handleList(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "list")
}

func (rh *remoteReadHandler) handleStat(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "stat")
}

func (rh *remoteReadHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	rh.serve(w, r, "download")
}

// serve 统一走「授权 → 委派 → 审计」三步；拒绝路径在 authorize 内记审计后直接返回。
func (rh *remoteReadHandler) serve(w http.ResponseWriter, r *http.Request, op string) {
	tgt, ok := rh.authorize(w, r)
	if !ok {
		return
	}
	sw := &remoteStatusWriter{ResponseWriter: w}
	rh.delegate(sw, r, tgt, op)
	rh.h.RecordAudit(r.Context(), AuditEvent{
		Action: "mesh_read", Actor: tgt.owner, Mesh: tgt.node,
		ObjectType: "file", Object: tgt.path,
		Result: remoteAuditResult(sw.status),
		Detail: "volume=" + tgt.vol.Name + " path=" + tgt.path + " status=" + strconv.Itoa(sw.status),
	})
}

// authorize 解析并校验授权；不通过时写 404/401 并记审计，返回 ok=false。
//
// 授权判定必须同时调用两个方法，二者缺一不可：
//   - MeshReaderFor(fp) 只做指纹反查，取回 (node, owner) 绑定——owner 由此而来，
//     绝不由请求参数决定（防越权枚举的红线）；
//   - AuthorizeMeshRead(node, fp, owner) 施加**第二重约束**：三元组命中且该 owner
//     本身过本卷 ACL。
//
// 特别注意：MeshReaderFor **不得**作为唯一授权依据——它只查指纹，可能返回一个
// owner 已被本卷 ACL 拉黑的绑定条目（AuthorizeMeshRead 文档同此结论）。
func (rh *remoteReadHandler) authorize(w http.ResponseWriter, r *http.Request) (*remoteTarget, bool) {
	volName := strings.TrimSpace(r.URL.Query().Get("volume"))
	// path 允许远端「以 / 开头的绝对写法」（如 /docs/a.txt，与 sclient `cd /` 的
	// 根语义一致）；此处归一为 owner user 桶内相对路径。既有的 ValidateFilePath
	// 会拒绝绝对路径，故 list/download/stat 三条路径都在此统一去首斜杠，避免
	// 同一 path 在 list（listFiles 自带 TrimPrefix）与 download/stat 上语义分叉。
	relPath := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("path")), "/")

	denied := func(result, detail string) (*remoteTarget, bool) {
		rh.h.RecordAudit(r.Context(), AuditEvent{
			Action: "mesh_read", ObjectType: "file", Object: relPath,
			Result: result, Detail: "volume=" + volName + " path=" + relPath + ": " + detail,
		})
		writeRemoteError(w, http.StatusNotFound, "not found")
		return nil, false
	}

	fp := ""
	if rh.peer != nil {
		fp = strings.TrimSpace(rh.peer.PeerFingerprint())
	}
	if fp == "" {
		// 未完成身份握手（理论上不可达：B 侧 listener 恒用非 nil 静态密钥 →
		// Serve 已强制握手）。防御性拒绝，且不泄露任何卷/文件存在性。
		rh.h.RecordAudit(r.Context(), AuditEvent{
			Action: "mesh_read", ObjectType: "file", Object: relPath,
			Result: AuditResultDenied, Detail: "volume=" + volName + " path=" + relPath + ": 对端无已认证身份指纹",
		})
		writeRemoteError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if volName == "" {
		return denied(AuditResultDenied, "缺少 volume 参数")
	}
	if rh.h.volSet == nil {
		return denied(AuditResultError, "卷集合未装配")
	}
	vol, ok := rh.h.volSet.ByName(volName)
	if !ok {
		return denied(AuditResultDenied, "卷不存在")
	}
	mr, ok := vol.MeshReaderFor(fp)
	if !ok {
		return denied(AuditResultDenied, "指纹未列入本卷 mesh_readers")
	}
	if !vol.AuthorizeMeshRead(mr.Node, fp, mr.Owner) {
		return denied(AuditResultDenied, "授权三元组未通过 node="+mr.Node)
	}
	return &remoteTarget{vol: vol, node: mr.Node, owner: mr.Owner, path: relPath}, true
}

// delegate 把远程请求重写为既有内部读请求并委派既有 handler（AD-8）：
//   - owner 经受限 context 注入（复用 actorCtxKey 通路，使 ActorFrom(ctx) 返回该 owner）；
//   - 卷显式锁定（?volume=），owner 的 user 桶内相对路径改写为既有 `subdir`/`filename`。
//
// 请求方自带的 owner/actor 等查询参数一律被丢弃：r2 的 query 由本函数从零重建，
// 只含 volume 与 subdir（list）或 filename（stat/download）两个键。
//
// 既有的 ValidateFilePath / 多卷 ACL / 跨卷定位等校验全部保留。
func (rh *remoteReadHandler) delegate(w http.ResponseWriter, r *http.Request, tgt *remoteTarget, op string) {
	q := url.Values{}
	q.Set("volume", tgt.vol.Name)
	switch op {
	case "list":
		q.Set("subdir", tgt.path)
	case "stat", "download":
		q.Set("filename", tgt.path)
	}

	r2 := r.Clone(withActor(r.Context(), tgt.owner))
	r2.URL = &url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	r2.RequestURI = r2.URL.RequestURI()

	switch op {
	case "list":
		rh.h.listFiles(w, r2)
	case "stat":
		rh.h.stat(w, r2)
	case "download":
		rh.h.download(w, r2)
	}
}

// remoteStatusWriter 记录响应状态码，供审计落「放行/不存在/错误」。
type remoteStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *remoteStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *remoteStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// remoteAuditResult 把响应状态映射为既有审计结果常量。
func remoteAuditResult(status int) string {
	switch status {
	case 0, http.StatusOK, http.StatusPartialContent:
		return AuditResultSuccess
	default:
		return AuditResultError
	}
}

// writeRemoteError 写纯文本错误响应（不泄露卷/文件存在性）。
func writeRemoteError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}
