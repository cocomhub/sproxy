// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"strings"
	"testing"
)

// TestValidate_OK 验证合法字段全部通过（A/B/C 三档代表字段）。
func TestValidate_OK(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"code":           "MOVIE-001",
		"maker":          "STUDIO X",
		"maker_slug":     "studio-x",
		"series":         "夜行バス",
		"actress":        []string{"葵ぶるま"},
		"genres":         []string{"ドラマ"},
		"release_date":   "2026-09-17",
		"title_ja":       "MOVIE-001 夜行バス",
		"page_url":       "https://example.com/ja/movie-001",
		"site":           "example",
		"origin_name":    "MOVIE-001.avi",
		"source":         "cloud_share",
		"share_url":      "https://example.com/s/abc",
		"share_id":       "sh-12345",
		"share_hash":     "AABBCCDD11223344556677889900",
		"mirror_urls":    []string{"https://mirror.example.com/a"},
		"duration_sec":   float64(8956),
		"resolution":     "720p",
		"uncensored":     true,
		"subtitle_files": []string{"MOVIE-001.srt"},
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("合法字段应通过: %v", err)
	}
}

// TestValidate_UnknownField 验证白名单外字段被拒（防任意键注入）。
func TestValidate_UnknownField(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"evil_key": "x"})
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("未知字段应拒绝, err=%v", err)
	}
}

// TestValidate_TooLong 验证超长字符串被拒（单字段上限）。
func TestValidate_TooLong(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"code": strings.Repeat("x", 100)})
	if err == nil || !strings.Contains(err.Error(), "超长") {
		t.Fatalf("超长应拒绝, err=%v", err)
	}
}

// TestValidate_TooLarge 验证总大小超 16 KiB 被拒（防滥用）。
func TestValidate_TooLarge(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"origin_name": strings.Repeat("y", 300)})
	if err != nil {
		t.Fatalf("单字段 300B 应通过: %v", err)
	}
	// 构造超大：多条 500B 数组元素
	big := make([]string, 40)
	for i := range big {
		big[i] = strings.Repeat("z", 500)
	}
	err = ValidateExtra(map[string]any{"mirror_urls": big})
	if err == nil || !strings.Contains(err.Error(), "超上限") {
		t.Fatalf("总体超限应拒绝, err=%v", err)
	}
}

// TestValidate_WrongType 验证类型不匹配被拒。
func TestValidate_WrongType(t *testing.T) {
	t.Parallel()
	cases := []map[string]any{
		{"code": 123},
		{"actress": "not-array"},
		{"uncensored": "true"},
		{"duration_sec": "8956"},
	}
	for _, c := range cases {
		if err := ValidateExtra(c); err == nil {
			t.Fatalf("类型错误应拒绝: %v", c)
		}
	}
}

// TestValidate_Enum 验证枚举字段（source/resolution）非法值被拒。
func TestValidate_Enum(t *testing.T) {
	t.Parallel()
	for _, c := range []map[string]any{
		{"source": "torrent"},
		{"resolution": "2k"},
	} {
		if err := ValidateExtra(c); err == nil {
			t.Fatalf("枚举外取值应拒绝: %v", c)
		}
	}
}

// TestValidate_JSONDecoded 验证 JSON 反序列化形态（[]any / float64）被接受。
func TestValidate_JSONDecoded(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"actress":      []any{"葵ぶるま", "椿まひる"},
		"duration_sec": float64(8956),
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("JSON 形态应通过: %v", err)
	}
}

// TestTrim 验证空白字符串被剔除（不污染 meta）。
func TestTrim(t *testing.T) {
	t.Parallel()
	got := TrimExtra(map[string]any{"code": "  MOVIE-001  ", "maker": "   ", "actress": []string{"x"}})
	if got["code"] != "MOVIE-001" {
		t.Fatalf("code 应 trim: %v", got["code"])
	}
	if _, ok := got["maker"]; ok {
		t.Fatalf("空白字符串应剔除")
	}
	if _, ok := got["actress"]; !ok {
		t.Fatalf("非字符串字段应保留")
	}
}

// TestValidateExtra_CustomPrefix 验证 `x_` 前缀自定义扩展字段：
// 白名单外键可携带（任意语义），但类型/键名/总量仍受限。
func TestValidateExtra_CustomPrefix(t *testing.T) {
	t.Parallel()
	// 合法：x_ 自定义字段
	if err := ValidateExtra(map[string]any{"code": "EXAMPLE-001", "x_my_site_meta": "abc"}); err != nil {
		t.Fatalf("x_ 字段应通过: %v", err)
	}
	// 非法：非 x_ 前缀的白名单外键 → 拒绝
	if err := ValidateExtra(map[string]any{"random_key": 1}); err == nil {
		t.Fatal("白名单外非 x_ 键应拒绝")
	}
	// 非法：x_ 键名含大写 → 拒绝
	if err := ValidateExtra(map[string]any{"x_MyKey": 1}); err == nil {
		t.Fatal("x_ 键名大写应拒绝")
	}
	// 非法：x_ 值类型不支持（struct）→ 拒绝
	if err := ValidateExtra(map[string]any{"x_bad": struct{ A int }{1}}); err == nil {
		t.Fatal("x_ 值不支持 struct 应拒绝")
	}
	// 合法：x_ 布尔/数字/[]string
	if err := ValidateExtra(map[string]any{"x_enabled": true, "x_count": float64(3), "x_tags": []string{"a"}}); err != nil {
		t.Fatalf("x_ 标量应通过: %v", err)
	}
}

// TestTrimExtra_CustomPreserved 验证 TrimExtra 保留 x_ 字段（不被当空值裁剪）。
func TestTrimExtra_CustomPreserved(t *testing.T) {
	t.Parallel()
	in := map[string]any{"code": "X-1", "x_foo": "", "x_bar": "keep", "site": ""}
	out := TrimExtra(in)
	if _, ok := out["x_foo"]; ok {
		t.Fatal("x_ 空字符串应裁剪（与白名单同语义）")
	}
	if out["x_bar"] != "keep" {
		t.Fatalf("x_bar = %v, want keep", out["x_bar"])
	}
	if _, ok := out["site"]; ok {
		t.Fatal("白名单空值也应裁剪")
	}
}

// TestValidateExtra_TransferBusinessKeys 锁定「业务层（sdserver）实际写入的 meta_extra 键集」
// 全部通过校验（含本轮新增的 resolution/origin_name/share_id/share_file_id/subtitle_files
// 与 x_ 扩展）。防两仓键集漂移：业务侧写得出、服务端拒写（400/静默丢弃）。
func TestValidateExtra_TransferBusinessKeys(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"code": "EXAMPLE-001", "title_ja": "EXAMPLE-001 タイトル", "title_cn": "示例标题",
		"release_date": "2026-01-02", "maker": "メーカー", "series": "シリーズ",
		"label": "レーベル", "director": "監督",
		"actress": []string{"女優A"}, "genres": []string{"ジャンル"},
		"duration_sec": int64(3600), "uncensored": true, "has_cn_sub": true,
		"page_url": "https://example.com/ja/example-001", "site": "example",
		"share_url": "https://example.com/share/x", "share_hash": "GCID-X",
		"share_file_name": "EXAMPLE-001.mp4", "share_file_id": "f-1", "share_id": "s-1",
		"origin_name": "EXAMPLE-001.mp4", "resolution": "1080p",
		"subtitle_files": []string{"EXAMPLE-001.srt"},
		// x_ 扩展（双语/角色/封面/来源/内容分组）
		"x_maker_cn": "厂牌", "x_series_cn": "系列", "x_label_cn": "标签",
		"x_director_cn": "导演", "x_release_date_cn": "2026年1月2日",
		"x_actress_cn": []string{"女优A"}, "x_genres_cn": []string{"类型"},
		"x_cover_url": "https://example.com/cover.jpg", "x_has_en_sub": true,
		"x_content_group": "EXAMPLE-001", "x_type": "composite", "x_role": "video",
		"x_share_source_url": "https://example.com/keepshare/x",
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("业务 meta_extra 键集校验失败（键集漂移）: %v", err)
	}
}

// TestValidateExtra_DownloadAuditKeys 锁定云端转存写入的下载审计键（x_download_*/x_source_*）
// 属于合法自定义扩展（避免审计数据在 upload 校验路径被拒）。
func TestValidateExtra_DownloadAuditKeys(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"source":                     "transfer",
		"checksum_algorithm":         "sha256",
		"authority_hash_algorithm":   "pikpak-gcid",
		"downloaded_at":              "2026-10-28T02:01:30Z",
		"x_source_created_at":        "2026-10-20T01:02:03Z",
		"x_source_modified_at":       "2026-10-21T04:05:06Z",
		"x_download_started_at":      "2026-10-28T02:00:00Z",
		"x_download_finished_at":     "2026-10-28T02:01:30Z",
		"x_download_duration_ms":     int64(90000),
		"x_download_anonymous_bytes": int64(100),
		"x_download_account_bytes":   int64(200),
		"x_download_bytes_per_sec":   int64(3),
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("下载审计键校验失败: %v", err)
	}
}

// TestValidateExtra_TransferSourceKeys 锁定转存路径写入的「来源」键必须可往返：
// `source_url`（来源链接）与 `transferred_at`（转存完成时刻）由服务端
// buildTransferSourceMeta 写入 —— 若不在白名单，客户端就无法按源 1:1 补录/迁移
// （跨租户搬迁、灾后重建都会丢这两个字段）。覆盖审计 2026-10-28。
func TestValidateExtra_TransferSourceKeys(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"source":         "transfer",
		"source_url":     "https://example.com/share?spx_target=EXAMPLE-001.mp4",
		"transferred_at": "2026-10-28T13:27:46Z",
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("转存来源键应通过校验（否则无法 1:1 补录）: %v", err)
	}
	// 类型仍受约束：非字符串拒绝
	if err := ValidateExtra(map[string]any{"source_url": int64(1)}); err == nil {
		t.Fatal("source_url 非字符串应拒绝")
	}
	if err := ValidateExtra(map[string]any{"transferred_at": true}); err == nil {
		t.Fatal("transferred_at 非字符串应拒绝")
	}
}

// TestValidateExtra_ManualUploadSource 锁定 source 枚举含 manual_upload（人工下载后上传的归档）。
// 语义：transfer=服务端自动转存；manual_upload=人工下载后上传（内容与分享一致，由人工搬运）。
func TestValidateExtra_ManualUploadSource(t *testing.T) {
	t.Parallel()
	if err := ValidateExtra(map[string]any{"source": "manual_upload"}); err != nil {
		t.Fatalf("source=manual_upload 应通过: %v", err)
	}
	if err := ValidateExtra(map[string]any{"source": "manual"}); err == nil {
		t.Fatal("枚举外值应拒绝（防拼写漂移）")
	}
}
