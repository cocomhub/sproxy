// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxysig

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testAK = "ak-prod-meshA-3f8a"
	testSK = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var baseTime = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// buildHeader 用 SK 计算合法签名的请求头（时间以 baseTime 为基准偏移）。
func buildHeader(ak, sk, nonce string, tsOffset, ttl time.Duration, method, path, query, bodyHash string) Header {
	ts := baseTime.Add(tsOffset).UnixMilli()
	exp := baseTime.Add(tsOffset + ttl).UnixMilli()
	h := Header{Version: Version, AK: ak, TS: ts, Exp: exp, Nonce: nonce, BodySHA256: bodyHash}
	h.Sig = Sign(sk, h, method, path, query)
	return h
}

// buildHeaderV2 构造带 EntryID 的 v2 请求头（方法/路径/查询与签名均绑定）。
func buildHeaderV2(ak, sk, entryID, nonce string, tsOffset, ttl time.Duration, method, path, query, bodyHash string) Header {
	h := buildHeader(ak, sk, nonce, tsOffset, ttl, method, path, query, bodyHash)
	h.EntryID = entryID
	h.Sig = Sign(sk, h, method, path, query)
	return h
}

// headerAuthString 按当前 header 字段渲染 Authorization 头值。
// 供 ParseHeader 断言使用（必须与 SignAndFormat 的渲染顺序一致）。
func headerAuthString(h Header) string {
	return Scheme + " v=" + h.Version + " ak=" + h.AK + " sk=" + h.EntryID +
		" ts=" + itoa(h.TS) + " exp=" + itoa(h.Exp) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
}

func TestSignVerify_HappyPath(t *testing.T) {
	h := buildHeader(testAK, testSK, "nonce-1", 0, DefaultExpiry, "POST", "/upload", "", EmptyBodyHash())
	// now 在有效期内（ts+2min）。
	now := baseTime.Add(2 * time.Minute)
	seen := false
	err := Verify(testSK, h, "POST", "/upload", "", now, DefaultMaxTTL, DefaultClockSkew, func(_, _ string, _ int64) bool { return seen })
	if err != nil {
		t.Fatalf("Verify 应通过, got %v", err)
	}
}

func TestVerify_Rejections(t *testing.T) {
	now := baseTime.Add(2 * time.Minute)
	cases := []struct {
		name string
		h    Header
		sk   string
		want error
	}{
		{"未来时间", func() Header {
			h := buildHeader(testAK, testSK, "n", 10*time.Minute, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			return h
		}(),
			testSK, ErrFuture},
		{"TTL 超上限", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, 30*time.Minute, "GET", "/a", "", EmptyBodyHash())
			return h
		}(),
			testSK, ErrTTLTooLong},
		{"签名错误", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			h.Sig = strings.Repeat("0", 64)
			return h
		}(),
			testSK, ErrBadSignature},
		{"错误 SK", buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash()),
			strings.Repeat("f", 64), ErrBadSignature},
		{"版本不支持", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			h.Version = "3"
			return h
		}(),
			testSK, ErrVersion},
		{"nonce 重放", buildHeader(testAK, testSK, "dup", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash()),
			testSK, ErrReplay},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify(c.sk, c.h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, func(_, n string, _ int64) bool { return n == "dup" })
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v, got %v", c.want, err)
			}
		})
	}
}

func TestVerify_Expired(t *testing.T) {
	// exp = ts + DefaultExpiry = +5min；now = +6min → 已过期。
	h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
	now := baseTime.Add(6 * time.Minute)
	if err := Verify(testSK, h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("期望 ErrExpired, got %v", err)
	}
}

func TestVerify_ClientShorterTTL_Allowed(t *testing.T) {
	// 客户端声明 30s（比默认短），受 max_ttl 约束内 → 通过。
	h := buildHeader(testAK, testSK, "n", 0, 30*time.Second, "GET", "/a", "", EmptyBodyHash())
	now := baseTime.Add(10 * time.Second)
	if err := Verify(testSK, h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, nil); err != nil {
		t.Fatalf("短 TTL 应通过, got %v", err)
	}
}

// TestVerify_V2_WithEntryID：v2 头带 sk=<entryID> 段，canonical 含 entryID 段，验签通过。
func TestVerify_V2_WithEntryID(t *testing.T) {
	h := buildHeaderV2(testAK, testSK, "sk-abcdef012345", "nonce-v2-1", 0, DefaultExpiry, "POST", "/upload", "", EmptyBodyHash())
	if h.Version != Version {
		t.Fatalf("buildHeaderV2 Version 应为 %q, got %q", Version, h.Version)
	}
	// canonical 必须包含 entryID 段（AK 段之后）。
	wantLine := "sproxy-sig/v" + Version + "\n" + testAK + "\n" + "sk-abcdef012345"
	if got := h.Canonical("POST", "/upload", ""); !strings.HasPrefix(got, wantLine+"\n") {
		t.Fatalf("canonical 应含 entryID 段:\ngot  %q\nwant 前缀 %q", got, wantLine)
	}
	now := baseTime.Add(2 * time.Minute)
	if err := Verify(testSK, h, "POST", "/upload", "", now, DefaultMaxTTL, DefaultClockSkew, nil); err != nil {
		t.Fatalf("带 entryID 的 v2 验签应通过, got %v", err)
	}
}

// TestVerify_V2_NoEntryID：EntryID 为空 → 校验走空段（空行），通过。
func TestVerify_V2_NoEntryID(t *testing.T) {
	h := buildHeaderV2(testAK, testSK, "", "nonce-v2-2", 0, DefaultExpiry, "POST", "/upload", "", EmptyBodyHash())
	// canonical 中 entryID 段为空（空行，紧接 AK 段后）。
	wantPrefix := "sproxy-sig/v" + Version + "\n" + testAK + "\n\n"
	if got := h.Canonical("POST", "/upload", ""); !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("空 entryID 应为空行段:\ngot  %q\nwant 前缀 %q", got, wantPrefix)
	}
	now := baseTime.Add(2 * time.Minute)
	if err := Verify(testSK, h, "POST", "/upload", "", now, DefaultMaxTTL, DefaultClockSkew, nil); err != nil {
		t.Fatalf("空 entryID 的 v2 验签应通过, got %v", err)
	}
}

// TestVerify_Reject_VersionMismatch：明文 v=1 头在校验时被拒（服务端仅支持 v2）。
func TestVerify_Reject_VersionMismatch(t *testing.T) {
	// 构造明文 v=1 头：与 v2 相同的字段、仅版本号前缀不同 → 校验必须拒绝。
	auth := Scheme + " v=1 ak=" + testAK + " ts=1784808000123 exp=1784808300123 nonce=deadbeef body_sha256=" + EmptyBodyHash() + " sig=AABB"
	h, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	now := baseTime.Add(2 * time.Minute)
	if err := Verify(testSK, h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, nil); !errors.Is(err, ErrVersion) {
		t.Fatalf("v=1 头应拒绝为 ErrVersion, got %v", err)
	}
}

func TestParseHeader(t *testing.T) {
	h := buildHeaderV2(testAK, testSK, "sk-abcdef012345", "nonce-9", 0, DefaultExpiry, "PUT", "/api/x", "a=1", BodyHash([]byte("hi")))
	parsed, err := ParseHeader(headerAuthString(h))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if parsed != h {
		t.Fatalf("解析结果不一致: %+v vs %+v", parsed, h)
	}
}

// TestParseHeader_V2_EntryID：v2 头带 sk=<entryID> 段解析正确（EntryID 填充）。
func TestParseHeader_V2_EntryID(t *testing.T) {
	auth := Scheme + " v=2 ak=sk-prod-meshA-3f8a sk=sk-abcdef012345" +
		" ts=1784808000123 exp=1784808300123 nonce=deadbeef body_sha256=" + EmptyBodyHash() + " sig=AABB"
	h, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Version != "2" || h.EntryID != "sk-abcdef012345" {
		t.Fatalf("解析 ak/entryID 错误: %+v", h)
	}
}

// TestSignAndFormat_EntryID_RoundTrip：SignAndFormat 渲染的带 sk=<id> 头必须能被
// ParseHeader 还原出正确的 EntryID，且能通过 Verify（canonical 与渲染一致性）。
// 回归：历史 bug 曾用 Replace(" ak=", " ak="+AK+" sk="+ID) 锚点错误，把 EntryID
// 粘连到 AK 值后形成 sk=<id><AK>，ParseHeader 解析出的 EntryID 错误、验签失配。
// 真实消费路径（pkg/client.signRequest）的 v2 签名依赖此一致性。
func TestSignAndFormat_EntryID_RoundTrip(t *testing.T) {
	const (
		ak = "ak-prod-meshA-3f8a"
		id = "sk-abcdef012345"
	)
	h := buildHeader(ak, testSK, "nonce-e1", 0, DefaultExpiry, "POST", "/api/credentials/"+ak+"/renew", "", EmptyBodyHash())
	h.EntryID = id
	auth := SignAndFormat(testSK, h, "POST", "/api/credentials/"+ak+"/renew", "")
	parsed, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("ParseHeader(%s): %v", auth, err)
	}
	if parsed.AK != ak {
		t.Errorf("AK = %q, want %q（sk= 段不得粘连进 AK）", parsed.AK, ak)
	}
	if parsed.EntryID != id {
		t.Errorf("EntryID = %q, want %q（sk= 段值必须只含 entryID 本身）", parsed.EntryID, id)
	}
	// Verify 用 baseTime 偏移后的 h 时间（避开真实 now 过期窗口）。
	parsed2 := parsed
	parsed2.TS = baseTime.UnixMilli()
	parsed2.Exp = baseTime.Add(DefaultExpiry).UnixMilli()
	parsed2.Sig = Sign(testSK, parsed2, "POST", "/api/credentials/"+ak+"/renew", "")
	if err := Verify(testSK, parsed2, "POST", "/api/credentials/"+ak+"/renew", "", baseTime.Add(time.Minute), 0, 0, nil); err != nil {
		t.Errorf("Verify 失败（canonical 与渲染不一致）: %v", err)
	}
}

// TestParseHeader_V2_NoEntryID：v2 头缺省 sk=<entryID> 段时 EntryID 为空串、其余解析正确。
func TestParseHeader_V2_NoEntryID(t *testing.T) {
	h := buildHeader(testAK, testSK, "nonce-9", 0, DefaultExpiry, "PUT", "/a", "", EmptyBodyHash())
	auth := Scheme + " v=" + h.Version + " ak=" + h.AK + " ts=" + itoa(h.TS) + " exp=" + itoa(h.Exp) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
	parsed, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if parsed.EntryID != "" {
		t.Fatalf("缺省 sk 段时 EntryID 应为空, got %q", parsed.EntryID)
	}
	if parsed.AK != h.AK || parsed.BodySHA256 != h.BodySHA256 || parsed.Version != h.Version {
		t.Fatalf("其余字段解析不一致: %+v vs %+v", parsed, h)
	}
}

// TestParseHeader_Malformed_EmptySKAlias：显式 sk= 空值按字段空值 fail-closed 拒绝。
func TestParseHeader_Malformed_EmptySKAlias(t *testing.T) {
	if _, err := ParseHeader("SproxySig v=2 ak=sk-prod-meshA-3f8a sk= ts=1784808000123 exp=1784808300123 nonce=n body_sha256=" + EmptyBodyHash() + " sig=s"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("sk= 空值应 ErrMalformed, got %v", err)
	}
}

func TestParseHeader_Malformed(t *testing.T) {
	bad := []string{
		"",                      // 空
		"Bearer abc",            // 非 SproxySig
		"SproxySig",             // 缺空格后字段
		"SproxySig v=2 ak=only", // 缺字段
		"SproxySig v=2 ak=x ts=abc exp=1 nonce=n body_sha256=b sig=s",         // ts 非数字
		"SproxySig v=2 ak=x ts=1 exp=1 nonce=n body_sha256=b sig=s unknown=k", // 未知字段
		"SproxySig v=2 ts=1 exp=1 nonce=n body_sha256=b sig=s",                // 缺 ak
	}
	for _, s := range bad {
		if _, err := ParseHeader(s); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseHeader(%q) 期望 ErrMalformed, got %v", s, err)
		}
	}
}

// TestParseHeader_V1_Deprecated：v1 头能解析（不拒绝明文），但 Verify 按版本不匹配拒绝。
func TestParseHeader_V1_Deprecated(t *testing.T) {
	auth := Scheme + " v=1 ak=ak-prod-meshA-3f8a ts=1784808000123 exp=1784808300123 nonce=deadbeef body_sha256=" + EmptyBodyHash() + " sig=AABB"
	h, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("v1 头应可解析（Version 字段透出）, got %v", err)
	}
	if h.Version != "1" {
		t.Fatalf("v1 头 Version 应为 '1', got %q", h.Version)
	}
}

func TestBodyValidator(t *testing.T) {
	// 匹配：EOF 返回 io.EOF，无错误。
	declared := BodyHash([]byte("hello"))
	vr := NewBodyValidator(strings.NewReader("hello"), declared)
	if _, err := io.ReadAll(vr); err != nil {
		t.Fatalf("匹配 body 应通过, got %v", err)
	}
	// 不匹配：EOF 时返回错误。
	vr = NewBodyValidator(strings.NewReader("hello"), BodyHash([]byte("world")))
	if _, err := io.ReadAll(vr); err == nil {
		t.Fatal("不匹配 body 应报错")
	}
	// UNSIGNED：跳过比对。
	vr = NewBodyValidator(strings.NewReader("anything"), UnsignedBody)
	if _, err := io.ReadAll(vr); err != nil {
		t.Fatalf("UNSIGNED 应跳过比对, got %v", err)
	}
}

func TestNoncePool(t *testing.T) {
	p := NewNoncePool()
	exp := time.Now().Add(5 * time.Minute).UnixMilli()
	if p.Seen("ak1", "n1", exp) {
		t.Fatal("首次 nonce 不应判重放")
	}
	if !p.Seen("ak1", "n1", exp) {
		t.Fatal("重复 nonce 应判重放")
	}
	if p.Seen("ak1", "n2", exp) {
		t.Fatal("不同 nonce 不应判重放")
	}
	if p.Seen("ak2", "n1", exp) {
		t.Fatal("不同 ak 的 nonce 不应判重放")
	}
	// 空值 fail-closed。
	if !p.Seen("", "n", exp) || !p.Seen("ak", "", exp) {
		t.Fatal("空 ak/nonce 应 fail-closed 判重放")
	}
}

// TestHeader_EntryIDJSONTag：EntryID 的 json/yaml tag 为 "sk"，与线缆字段一致
// （供未来 Header 序列化 / 配置反序列化使用；编译期结构断言）。
func TestHeader_EntryIDJSONTag(t *testing.T) {
	h := Header{Version: "2", AK: "ak", EntryID: "sk-1"}
	b, err := marshalHeaderJSON(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 字段名必须是 sk（不是 entry_id/other）。
	if !strings.Contains(string(b), `"sk":"sk-1"`) {
		t.Fatalf("EntryID 应序列化为 sk 字段, got %s", b)
	}
	if strings.Contains(string(b), `"entry_id"`) || strings.Contains(string(b), `"EntryID"`) {
		t.Fatalf("EntryID 不得序列化为其它字段名, got %s", b)
	}
	// 反序列化：sk 值能回到 EntryID。
	got, err := unmarshalHeaderJSON(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EntryID != "sk-1" || got.AK != "ak" || got.Version != "2" {
		t.Fatalf("sk → EntryID 反序列化错误: %+v", got)
	}
}

func marshalHeaderJSON(h Header) ([]byte, error) {
	return json.Marshal(h)
}

func unmarshalHeaderJSON(b []byte) (Header, error) {
	var h Header
	err := json.Unmarshal(b, &h)
	return h, err
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestEmptyBodyHash(t *testing.T) {
	if got := EmptyBodyHash(); got != BodyHash(nil) {
		t.Fatalf("EmptyBodyHash=%q 应等于 sha256(空)=%q", got, BodyHash(nil))
	}
}

func TestBodyValidator_Unsigned(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), UnsignedBody)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "x" {
		t.Fatalf("UNSIGNED 应透传不校验: %v %q", err, b)
	}
}

func TestBodyValidator_Mismatch(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), BodyHash([]byte("y")))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("哈希不匹配应收错")
	}
}

// eofWithDataReader 在第一次 Read 同时返回数据与 io.EOF——io.Reader 允许 (n>0, io.EOF)，
// chunked 传输的底层 reader 常在最后一次 Read 这样返回。bodyValidator 不得丢弃这 n 字节。
type eofWithDataReader struct {
	data []byte
	done bool
}

func (r *eofWithDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, io.EOF
}

func TestBodyValidator_DataAndEOF(t *testing.T) {
	payload := "hello"
	r := NewBodyValidator(&eofWithDataReader{data: []byte(payload)}, BodyHash([]byte(payload)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("(n>0, io.EOF) 场景数据不得丢失: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("ReadAll 内容 = %q, want %q（末尾 chunk 被丢弃）", got, payload)
	}
}

// TestBodyValidator_DataAndEOF_Mismatch：最后一次 Read 同时返回数据与 EOF 且哈希不匹配时，
// 必须立即报错（不得延迟到后续 Read，否则调用方读到 (n, nil) 后停止读取会绕过校验）。
func TestBodyValidator_DataAndEOF_Mismatch(t *testing.T) {
	// 声明哈希是 "world"，实际 body 是 "hello"——即使 (n>0, io.EOF) 也必须立即拒绝。
	r := NewBodyValidator(&eofWithDataReader{data: []byte("hello")}, BodyHash([]byte("world")))
	// 单次 Read：应返回数据 + 错误（或 0 + 错误），但绝不能 (n, nil)。
	buf := make([]byte, 32)
	n, err := r.Read(buf)
	if err == nil {
		t.Fatalf("哈希不匹配应返回错误，got (n=%d, err=nil)", n)
	}
	if !strings.Contains(err.Error(), "哈希不匹配") {
		t.Fatalf("错误应包含'哈希不匹配', got %v", err)
	}
}
