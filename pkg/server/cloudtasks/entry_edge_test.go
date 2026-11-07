// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// entry_edge_test.go 补齐归档/组归档入口的**低频但可达**边界：输出目录准备失败、
// 请求 owner 与任务 owner 不同（转存输出租户缺失）、转存目标的 scheme 归一与加密后缀、
// SSRF 防护拒绝、以及错误映射函数的剩余字符串契约。
//
// 这些分支不属于常规成功路径，但都是「配置缺失/输入异常」时的既定响应，必须有锁：
// 否则一次无意的文案改写就会让客户端拿到错误的状态码。
package cloudtasks

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestArchiveOutput_MkdirFailure 归档输出目录无法创建（同名普通文件占位）⇒ 500 +
// 既有文案（不得静默回落到其它目录）。
func TestArchiveOutput_MkdirFailure(t *testing.T) {
	t.Parallel()

	t.Run("single task", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive", "blocker")
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{}`), "alice")

		if rec.Code != http.StatusInternalServerError || decodeArchive(t, rec).Message != msgArchiveDirFail {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("batch", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive", "blocker")
		rec := httptest.NewRecorder()

		env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"]}`), "alice")

		if rec.Code != http.StatusInternalServerError || decodeArchive(t, rec).Message != msgArchiveDirFail {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestArchiveBatch_DefaultName 未指定归档名时按时间戳派生（保证唯一），且落盘成功。
func TestArchiveBatch_DefaultName(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	rec := httptest.NewRecorder()

	env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"]}`), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := decodeArchive(t, rec).File; !strings.HasPrefix(got, "cloud-batch-") || !strings.HasSuffix(got, ".tar.gz") {
		t.Fatalf("默认批量归档名 = %q", got)
	}
}

// TestArchiveGroup_PrepTargetFailures 组归档输出侧失败：
// 请求 owner 的租户不可用 ⇒ 500（源收集用的是任务 owner，两条解析路径独立）；
// 输出目录无法创建 ⇒ 500。
func TestArchiveGroup_PrepTargetFailures(t *testing.T) {
	t.Parallel()

	t.Run("output tenant unavailable", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		// 任务 owner = alice（可解析源），请求 owner = bob（输出租户缺失）。
		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "bob")

		if rec.Code != http.StatusInternalServerError || decodeArchive(t, rec).Message != msgArchiveDirFail {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("mkdir failure", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive", "blocker")
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		if rec.Code != http.StatusInternalServerError || decodeArchive(t, rec).Message != msgArchiveDirFail {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestArchiveGroupToVolume_OpenFailure 组归档转存：源打开失败必须 500（不进入转存核心，
// 避免「无条目也写空归档」）。
func TestArchiveGroupToVolume_OpenFailure(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	sources := []ArchiveSource{{Root: env.root, Rel: "cloud/t1/missing.bin", TarRel: "t1/missing.bin"}}
	rec := httptest.NewRecorder()

	env.svc.ArchiveGroupToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), "g1", "alice",
		sources, CloudArchiveRequest{TargetVolume: "vault", ArchiveName: "g.tar.gz"}, "g.tar.gz")

	if rec.Code != http.StatusInternalServerError || !strings.Contains(decodeArchive(t, rec).Message, "open source failed") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if env.groups.setArchiveFile != "" {
		t.Fatalf("失败不得登记组归档名：%q", env.groups.setArchiveFile)
	}
}

// TestArchiveToVolume_SchemeDefaultAndEncryptSuffix 转存目标的两处归一：
// scheme 为空按 local 处理（本地卷可用），加密产物名补 .aes（密文不伪装成可 gunzip 的 .tar.gz）。
func TestArchiveToVolume_SchemeDefaultAndEncryptSuffix(t *testing.T) {
	t.Parallel()

	t.Run("empty scheme is local", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t, func(e *transferEnv) { e.vols.scheme = "" })
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil),
			transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.HasPrefix(decodeArchive(t, rec).File, "local://") {
			t.Fatalf("URL scheme = %q，期望 local", decodeArchive(t, rec).File)
		}
	})

	t.Run("encrypted product gets aes suffix", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t, func(e *transferEnv) {
			e.targets.archiveKey = []byte("0123456789abcdef0123456789abcdef")
		})
		req := transferReq(strings.NewReader("hello"), 5)
		req.Encrypt = true
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), req, slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := decodeArchive(t, rec).File; !strings.HasSuffix(got, ".tar.gz.aes") {
			t.Fatalf("加密产物 URL = %q，期望 .tar.gz.aes 后缀", got)
		}
		if _, ok := env.mem.data("alice/user/archive/a.tar.gz.aes"); !ok {
			t.Fatal("加密产物必须落 .tar.gz.aes 键")
		}
	})
}

// TestArchiveTargetCharge_StatErrorIsZero 无物理足迹且卷内 Stat 出错时返回 0
// （不编造占用，避免删除侧做「未曾提交的释放」）。
func TestArchiveTargetCharge_StatErrorIsZero(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	env.mem.statErr = errStatFailed
	if got := env.svc.archiveTargetCharge("vault", "alice", "alice/user/archive/x.tar.gz", "archive/x.tar.gz"); got != 0 {
		t.Fatalf("Stat 失败时 = %d，want 0", got)
	}
}

// TestDeleteGroupArchive_ParseError 归档 URL 无法解析 ⇒ 错误（不得当作普通归档名删除）。
func TestDeleteGroupArchive_ParseError(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	if err := env.svc.DeleteGroupArchive("alice", "vault://%zz/x.tar.gz"); err == nil {
		t.Fatal("非法 URL 应返回错误")
	}
}

// TestGroupErrorStatus_RemainingContracts 组创建错误映射的剩余字符串契约：
// 协议未声明 / secrets 密钥卷属配置或输入错误（400），其余未知错误 500。
func TestGroupErrorStatus_RemainingContracts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want int
	}{
		{"转存协议未声明", http.StatusBadRequest},
		{"目标是 secrets 密钥卷，不可转存", http.StatusBadRequest},
		{"boom", http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := groupErrorStatus(errString(c.msg)); got != c.want {
			t.Errorf("groupErrorStatus(%q) = %d，want %d", c.msg, got, c.want)
		}
	}
}

// errString 是仅承载文案的错误（映射函数按子串判定，不依赖错误类型）。
type errString string

func (e errString) Error() string { return string(e) }

// TestValidateCloudDownloadURL_RejectsPrivateHost allowPrivate=false 时 SSRF 防护必须
// 拒绝解析到内网地址的 URL（组/单条/批量三条入口共用同一规则）。
func TestValidateCloudDownloadURL_RejectsPrivateHost(t *testing.T) {
	t.Parallel()
	if _, _, err := ValidateCloudDownloadURL("http://127.0.0.1/secret", "s.bin", false); err == nil {
		t.Fatal("allowPrivate=false 时必须拒绝内网 host")
	}
	if _, _, err := ValidateCloudDownloadURL("http://127.0.0.1/secret", "s.bin", true); err != nil {
		t.Fatalf("allowPrivate=true 时应放行：%v", err)
	}
}

// TestWithShareTarget_InvalidURLReturnsInput URL 无法解析时原样返回（不丢用户输入，
// 由后续下载器如实报错）。
func TestWithShareTarget_InvalidURLReturnsInput(t *testing.T) {
	t.Parallel()
	if got := withShareTarget("http://%zz/x", "t"); got != "http://%zz/x" {
		t.Fatalf("非法 URL 应原样返回，got %q", got)
	}
}

// TestGetGroup_SecondLookupLost 刷新后二次 GetGroup 丢失（并发删除）⇒ 404 而非空对象。
func TestGetGroup_SecondLookupLost(t *testing.T) {
	t.Parallel()
	group := &cloud.CloudTaskGroup{ID: "g1", Owner: "alice"}
	calls := 0
	groups := &stubGroups{getGroupHook: func(string) (*cloud.CloudTaskGroup, bool) {
		calls++
		if calls == 1 {
			return group, true
		}
		return nil, false
	}}
	svc := New(Deps{Groups: groups, Tasks: &stubTasks{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/groups/g1", nil)
	req.SetPathValue("id", "g1")

	svc.GetGroup(rec, req, "alice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] != msgGroupNotFound {
		t.Fatalf("error = %q", body["error"])
	}
}

// TestCreateGroup_PassesTransferAndLocalFlags 组创建的三参落点：transfer 通过 ACL 后
// 逐字下传子任务参数（组链最后一跳不得丢弃）。
func TestCreateGroup_PassesTransferAndLocalFlags(t *testing.T) {
	t.Parallel()
	groups := &stubGroups{submitResult: &cloud.CloudTaskGroup{ID: "g1"}}
	groups.groups = map[string]*cloud.CloudTaskGroup{"g1": {ID: "g1"}}
	svc := New(Deps{
		Tasks:   &stubTasks{allowPrivate: true, maxBatch: 10},
		Groups:  groups,
		Volumes: &stubVolumes{allowed: map[string]bool{"alice\x00vault": true}},
		Audit:   &stubAudit{},
	})
	rec := httptest.NewRecorder()
	body := `{"urls":[{"url":"https://example.org/a"}],"transfer":{"volume":"vault","path":"p/a.bin"},
		"download_local":true,"integrity_must_pass":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", strings.NewReader(body))

	svc.CreateGroup(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	p := groups.submittedParams
	if p.Transfer == nil || p.Transfer.Volume != "vault" || p.Transfer.Path != "p/a.bin" {
		t.Fatalf("转存参数 = %+v", p.Transfer)
	}
	if !p.DownloadLocal || !p.IntegrityMustPass || !p.Save {
		t.Fatalf("三参落点 = %+v（save 缺省应 true）", p)
	}
}

// 编译期锚点：本文件依赖的卷类型仍可零值构造为转存目标（ACL 零值 = 开放）。
var _ = volume.Volume{Name: "vault"}

// errStatFailed 是 Stat 故障注入错误（验证「不编造占用」）。
var errStatFailed = errors.New("stat failed")
