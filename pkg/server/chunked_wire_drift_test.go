// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/files"
)

// chunked_wire_drift_test.go 是**分块族 HTTP 契约**的跨侧漂移守卫（对应任务 5
// `response_drift_test.go` 同样手法，覆盖分块族全部参与方）。
//
// # 参与方枚举方法（靠搜索，不靠回忆）
//
// 以本族每个线上字段名逐仓检索（`grep -rln --include=*.go "\"<field>" pkg/ cmd/ internal/ test/`
// 与 `grep -rln --include=*.js "<field>" web/`——**让 grep 自己递归**，不用 shell glob，
// 见任务 5 §4.4 的 F15 教训），命中并确认参与本族契约的有：
//
//	1. pkg/files/service.go            服务端 DTO（产出方；本族权威定义，见该文件 HTTP 契约小节）
//	2. pkg/client/chunked.go           SDK：chunkedInitRequest / chunkedCompleteRequest（构造请求）、
//	                                   ChunkedUploadResult（解析 complete）、statusData（解析 status）
//	3. web/static/sclient/api/files.js JS 分块客户端：构造 init/chunk/complete 请求、解析各响应
//	4. web/static/upload.js / app.js   JS 上传 UI：读 upload_id 会话键与 status.missing_chunks
//	   （`web/static/sclient/api/*.js` 曾因 `**` 未开 globstar 被整层漏掉，故本次逐目录递归验过）
//	5. test/e2e_test.go                真二进制 e2e 侧**本地 DTO**（e2eChunkUploadResponse /
//	   e2eStatusResponse / e2eCompleteResponse，约 1055-1090 行）：经真实 HTTP 解析本族响应。
//	   匿名/包内类型无法反射比对，但它会**在 e2e 层捕获漂移**（字段改名 → 解析成零值 → 断言失败），
//	   故列入枚举；本轮（R34 回炉）审查指出首轮报告漏了此参与方。
//
// 未纳入守卫者：`pkg/testutil/syncmock/remote.go`（测试替身，inline map/匿名 struct，无类型可反射）
// 与 `pkg/testutil/mockserver/upload.go`（会话类型替身，不产 JSON）——均为测试专用，改动不破线上契约。
//
// # 守卫强度（诚实声明）
//
//   - 响应 DTO 与 `client.ChunkedUploadResult` 是**具名类型** ⇒ 反射逐字段比对（最强）。
//   - 服务端请求体已提为具名类型（files.ChunkedInitRequest/ChunkedCompleteRequest）⇒ 可反射；
//     SDK 侧请求结构未导出、status 响应为匿名结构 ⇒ 只能**按源码抽取/存在性**比对（较弱，
//     但改名/改 tag 仍会变红）。
//   - JS 侧无类型 ⇒ 同一份字段名做**存在性**断言（弱于反射，但把"JS 静默降级"挡在 CI 里）。
//   - 尚无跨语言共享语料（pkg/cloudfilename 式）——本文件的 JS 断言是该方向的第一步。

// ---- 响应 DTO 的冻结形状（独立复述；任一侧改动都会让本表漂移） ----

type frozenChunkedInitResponse struct {
	Success   bool   `json:"success"`
	UploadID  string `json:"upload_id,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	Message   string `json:"message,omitempty"`
}

type frozenChunkStatusResponse struct {
	Success       bool   `json:"success"`
	UploadID      string `json:"upload_id,omitempty"`
	ReceivedCount int    `json:"received_count,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	MissingChunks []int  `json:"missing_chunks,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
	FileChecksum  string `json:"file_checksum,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Message       string `json:"message,omitempty"`
}

type frozenUploadSessionInfo struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	TotalSize     int64  `json:"total_size"`
	ReceivedCount int    `json:"received_count"`
	TotalChunks   int    `json:"total_chunks"`
	FileChecksum  string `json:"file_checksum"`
	FileModTime   int64  `json:"file_mod_time"`
	Status        string `json:"status"`
}

type frozenChunkSessionsResponse struct {
	Success  bool                      `json:"success"`
	Message  string                    `json:"message,omitempty"`
	Sessions []frozenUploadSessionInfo `json:"sessions"`
}

type frozenChunkUploadResponse struct {
	Success     bool   `json:"success"`
	ChunkIndex  int    `json:"chunk_index"`
	ShouldRetry bool   `json:"should_retry,omitempty"`
	Message     string `json:"message,omitempty"`
}

type frozenChunkCompleteResponse struct {
	Success        bool   `json:"success"`
	Filename       string `json:"filename,omitempty"`
	FileChecksum   string `json:"file_checksum,omitempty"`
	Message        string `json:"message,omitempty"`
	MismatchChunks []int  `json:"mismatch_chunks,omitempty"`
}

type frozenChunkedInitRequest struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	TotalSize    int64  `json:"total_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileChecksum string `json:"file_checksum"`
	FileModTime  int64  `json:"file_mod_time"`
	Volume       string `json:"volume,omitempty"`
}

type frozenChunkedCompleteRequest struct {
	UploadID string `json:"upload_id"`
}

// TestChunkedResponses_FrozenWireShape 断言分块族响应 DTO 与冻结形状逐字段一致（名称/类型/tag），
// 并对代表性样本比对 Marshal 后的 JSON 键集（omitempty 行为随样本变化）。
func TestChunkedResponses_FrozenWireShape(t *testing.T) {
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkedInitResponse{}, files.ChunkedInitResponse{})
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkStatusResponse{}, files.ChunkStatusResponse{})
	assertSameJSONShape(t, "frozen", "chunked", frozenUploadSessionInfo{}, files.UploadSessionInfo{})
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkUploadResponse{}, files.ChunkUploadResponse{})
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkCompleteResponse{}, files.ChunkCompleteResponse{})
	// ChunkSessionsResponse 的 Sessions 元素类型不同名（冻结表用 frozenUploadSessionInfo），
	// 故只比字段名与 tag，不强求元素类型同一（元素契约已由上一行单独守卫）。
	compareSessionsEnvelope(t)

	// UploadResponse：本族写出的是**第二份定义**（pkg/server / pkg/files）。
	// 三份必须逐字节同形——任一方向漂移都会让客户端或 JS 静默降级。
	assertSameJSONShape(t, "server", "chunked", UploadResponse{}, files.UploadResponse{})
	for _, s := range []struct {
		name      string
		srv, chnk any
	}{
		{"failure", UploadResponse{Success: false, Message: "分块不存在"}, files.UploadResponse{Success: false, Message: "分块不存在"}},
		{"with-checksum", UploadResponse{Success: true, Message: "ok", Checksum: "deadbeef"}, files.UploadResponse{Success: true, Message: "ok", Checksum: "deadbeef"}},
	} {
		sb, err := json.Marshal(s.srv)
		if err != nil {
			t.Fatalf("%s: server Marshal: %v", s.name, err)
		}
		cb, err := json.Marshal(s.chnk)
		if err != nil {
			t.Fatalf("%s: chunked Marshal: %v", s.name, err)
		}
		if string(sb) != string(cb) {
			t.Fatalf("%s: UploadResponse 序列化不一致：server=%s chunked=%s", s.name, sb, cb)
		}
	}

	// 样本序列化：字段名与 omitempty 行为逐字节一致。
	samples := []struct {
		name string
		a, b any
	}{
		{"init-success", frozenChunkedInitResponse{Success: true, UploadID: "u1", ChunkSize: 4096, Message: "ok"},
			files.ChunkedInitResponse{Success: true, UploadID: "u1", ChunkSize: 4096, Message: "ok"}},
		{"complete-mismatch", frozenChunkCompleteResponse{Success: false, Filename: "a.bin", Message: "重传", MismatchChunks: []int{1, 2}},
			files.ChunkCompleteResponse{Success: false, Filename: "a.bin", Message: "重传", MismatchChunks: []int{1, 2}}},
		{"upload-retry", frozenChunkUploadResponse{Success: true, ChunkIndex: 3, ShouldRetry: true},
			files.ChunkUploadResponse{Success: true, ChunkIndex: 3, ShouldRetry: true}},
		{"status-partial", frozenChunkStatusResponse{Success: true, UploadID: "u1", ReceivedCount: 1, TotalChunks: 3, MissingChunks: []int{1, 2}},
			files.ChunkStatusResponse{Success: true, UploadID: "u1", ReceivedCount: 1, TotalChunks: 3, MissingChunks: []int{1, 2}}},
	}
	for _, s := range samples {
		ab, err := json.Marshal(s.a)
		if err != nil {
			t.Fatalf("%s: frozen Marshal: %v", s.name, err)
		}
		bb, err := json.Marshal(s.b)
		if err != nil {
			t.Fatalf("%s: chunked Marshal: %v", s.name, err)
		}
		if string(ab) != string(bb) {
			t.Fatalf("%s: 序列化不一致：frozen=%s chunked=%s", s.name, ab, bb)
		}
	}
}

// compareSessionsEnvelope 比对 ChunkSessionsResponse 外壳（字段名/类型/tag）；元素类型因冻结表
// 命名不同（frozenUploadSessionInfo）而只比字段形状。
func compareSessionsEnvelope(t *testing.T) {
	t.Helper()
	ft := reflect.TypeFor[frozenChunkSessionsResponse]()
	ct := reflect.TypeFor[files.ChunkSessionsResponse]()
	if ft.NumField() != ct.NumField() {
		t.Fatalf("ChunkSessionsResponse 字段数漂移：frozen=%d chunked=%d", ft.NumField(), ct.NumField())
	}
	for i := range ft.NumField() {
		ff, cf := ft.Field(i), ct.Field(i)
		if ff.Name != cf.Name || ff.Tag != cf.Tag {
			t.Fatalf("ChunkSessionsResponse 第 %d 个字段漂移：frozen=%s(%s) chunked=%s(%s)", i, ff.Name, ff.Tag, cf.Name, cf.Tag)
		}
	}
	if fe, ce := ft.Field(2).Type.Elem(), ct.Field(2).Type.Elem(); fe.NumField() != ce.NumField() {
		t.Fatalf("Sessions 元素字段数漂移：frozen=%d chunked=%d", fe.NumField(), ce.NumField())
	}
}

// TestChunkedInitContract_NoDriftAcrossServerSDKAndJS 把分块 init 的**请求体契约**钉在三方：
// 服务端解析结构（files.ChunkedInitRequest）↔ SDK 构造结构（pkg/client 的 chunkedInitRequest，
// 未导出 ⇒ 按源码抽取 tag）↔ JS 构造字段（web/static/sclient/api/files.js，按存在性断言）。
func TestChunkedInitContract_NoDriftAcrossServerSDKAndJS(t *testing.T) {
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkedInitRequest{}, files.ChunkedInitRequest{})
	assertSameJSONShape(t, "frozen", "chunked", frozenChunkedCompleteRequest{}, files.ChunkedCompleteRequest{})

	want := jsonTagList(t, reflect.TypeFor[files.ChunkedInitRequest]())
	sdk := structJSONTags(t, readRepoFile(t, "pkg/client/chunked.go"), "chunkedInitRequest")
	if strings.Join(want, ",") != strings.Join(sdk, ",") {
		t.Fatalf("init 请求契约漂移：chunked=%v clientSDK=%v", want, sdk)
	}

	js := readRepoFile(t, "web/static/sclient/api/files.js")
	assertKeysPresent(t, "web/static/sclient/api/files.js", js, want)

	wantComplete := jsonTagList(t, reflect.TypeFor[files.ChunkedCompleteRequest]())
	sdkComplete := structJSONTags(t, readRepoFile(t, "pkg/client/chunked.go"), "chunkedCompleteRequest")
	if strings.Join(wantComplete, ",") != strings.Join(sdkComplete, ",") {
		t.Fatalf("complete 请求契约漂移：chunked=%v clientSDK=%v", wantComplete, sdkComplete)
	}
}

// chunkedUploadResultClientOnlyFields 是 `client.ChunkedUploadResult` 相对服务端 complete 响应
// 契约的**已知客户端扩展字段**白名单（Go 字段名；必须显式登记才会放行）。
var chunkedUploadResultClientOnlyFields = []string{"UploadID", "TotalChunks"}

// TestChunkedResponses_NoDriftAgainstClientSDK 断言 SDK 侧的解析结构覆盖服务端 complete 响应
// 的每个契约字段（同名同 tag），且 SDK 侧多出的字段必须显式登记并 omitempty（不得混入线上 JSON）。
func TestChunkedResponses_NoDriftAgainstClientSDK(t *testing.T) {
	// ① complete 响应：SDK 的 ChunkedUploadResult 与服务端 DTO 的公共字段必须同名同 tag。
	st := reflect.TypeFor[files.ChunkCompleteResponse]()
	ct := reflect.TypeFor[client.ChunkedUploadResult]()
	byName := map[string]reflect.StructField{}
	for field := range ct.Fields() {
		byName[field.Name] = field
	}
	for sf := range st.Fields() {
		cf, ok := byName[sf.Name]
		if !ok {
			t.Fatalf("complete 响应字段 %s(%s) 在 SDK ChunkedUploadResult 中缺失——SDK 将静默解析为零值", sf.Name, sf.Tag)
		}
		if cf.Tag != sf.Tag {
			t.Fatalf("complete 响应字段 %s tag 漂移：chunked=%s client=%s", sf.Name, sf.Tag, cf.Tag)
		}
	}
	// ② SDK 侧扩展字段必须**显式登记**（下列白名单）。它们来自 SDK 自身的会话记录
	//    （upload_id / total_chunks 由 SDK 本地维护），服务端 complete 响应不含。
	//
	//    与 task 5 的上传响应守卫不同，此处**不要求** omitempty：`client.ChunkedUploadResult`
	//    是 SDK 返回给调用方的**结果对象**，`pkg/client` 从不 Marshal 它（实测 json.Marshal
	//    仅用于请求体：chunkedInitRequest / chunkedCompleteRequest），故其字段不可能进入线上
	//    JSON。要求 omitempty 会是无的放矢的约束。
	for name := range byName {
		if _, ok := reflect.TypeFor[files.ChunkCompleteResponse]().FieldByName(name); ok {
			continue
		}
		if !slices.Contains(chunkedUploadResultClientOnlyFields, name) {
			t.Fatalf("SDK 扩展字段 %s(%s) 未显式登记——若它可能上线，请核对契约后再登记", name, byName[name].Tag)
		}
	}

	// ③ status 响应（SDK 用匿名结构解析）：字段名存在性断言。
	sdkSrc := readRepoFile(t, "pkg/client/chunked.go")
	assertKeysPresent(t, "pkg/client/chunked.go(statusData)", sdkSrc, jsonTagList(t, reflect.TypeFor[files.ChunkStatusResponse]()))
}

// TestChunkedResponses_NoDriftAgainstJS 断言 JS 侧消费的分块响应字段在服务端 DTO 中均存在
// （JS 无类型，只能做存在性断言；字段改名会让 JS 静默降级而非报错，故必须挡在 CI 里）。
func TestChunkedResponses_NoDriftAgainstJS(t *testing.T) {
	clientJS := readRepoFile(t, "web/static/sclient/api/files.js")
	uploadJS := readRepoFile(t, "web/static/upload.js")

	// init 响应：JS 读 upload_id 判定 already_exists、读 chunk_size 校准分块大小。
	assertKeysPresent(t, "files.js(init 响应)", clientJS, []string{"upload_id", "chunk_size"})
	// chunk 响应：JS 读 should_retry 决定静默重试还是失败返回。
	assertKeysPresent(t, "files.js(chunk 响应)", clientJS, []string{"should_retry", "message"})
	// status 响应：JS 读 missing_chunks 决定续传哪些分片。
	assertKeysPresent(t, "files.js(status 响应)", clientJS, []string{"success", "missing_chunks"})
	// complete 响应：JS 读 file_checksum 与 message。
	assertKeysPresent(t, "files.js(complete 响应)", clientJS, []string{"file_checksum", "message"})
	// 上传 UI 另一条消费路径（upload.js 的续传探测）。
	assertKeysPresent(t, "upload.js(status 响应)", uploadJS, []string{"success", "missing_chunks"})
}

// ---- helpers ----

// jsonTagList 返回结构体各字段的 json tag 名（保序，含 omitempty 修饰前的名字）。
func jsonTagList(t *testing.T, rt reflect.Type) []string {
	t.Helper()
	out := make([]string, 0, rt.NumField())
	for field := range rt.Fields() {
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		out = append(out, name)
	}
	return out
}

var structBlockRe = func(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)type ` + regexp.QuoteMeta(name) + ` struct \{(.*?)\n\}`)
}
var jsonTagNameRe = regexp.MustCompile("json:\"([^\",]+)")

// structJSONTags 从 Go 源码中抽取 `type <name> struct { ... }` 的 json tag 名（保序）。
// 用于**未导出**结构（SDK 侧 chunkedInitRequest 等无法反射）的契约比对。
func structJSONTags(t *testing.T, src, typeName string) []string {
	t.Helper()
	m := structBlockRe(typeName).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("源码中未找到 type %s struct{...}（结构被改名/删除？读取的是 pkg/client/chunked.go）", typeName)
	}
	var out []string
	for _, tag := range jsonTagNameRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, tag[1])
	}
	if len(out) == 0 {
		t.Fatalf("type %s 未解析出任何 json tag", typeName)
	}
	return out
}

// assertKeysPresent 断言 keys 中每个名字都作为**独立词**出现在 src 中（词边界为「非标识符字符」）。
// 允许前置 `.`：JS 侧字段多经属性访问出现（`st.missing_chunks`），Go 侧经结构体字段访问出现。
// 用于 JS 源码与匿名解析结构的字段存在性检查。
func assertKeysPresent(t *testing.T, what, src string, keys []string) {
	t.Helper()
	for _, k := range keys {
		if !regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(k) + `([^\w]|$)`).MatchString(src) {
			t.Fatalf("%s 中未出现字段 %q——契约字段被改名/删除（JS/SDK 侧会静默降级）", what, k)
		}
	}
}

// readRepoFile 读取仓库内相对路径文件（go test 以包目录为 cwd，故从 pkg/server 上溯两级）。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", rel, err)
	}
	return string(b)
}
