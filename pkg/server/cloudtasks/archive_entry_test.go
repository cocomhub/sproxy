// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// archive_entry_test.go 补齐云归档入口（archive.go 单任务/批量、group_archive.go 组归档）
// 的分支矩阵：请求体/名称归一失败、源文件收集的逐条跳过原因、预留失败、对账失败
// （→ Release 或不提交）、同名冲突（O_EXCL 409）、转存目标 ACL，以及成功路径的
// 真实 tar.gz 产物与响应字段。
//
// 失败面按「状态码 + 文案」锁定；成功面校验「响应 checksum = 落盘产物的 SHA-256」
// 与「归档名落库/占用登记」两个不变量（客户端按 checksum 校验归档完整性）。
package cloudtasks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// perOwnerTenants 按 owner 解析租户（nil = 该 owner 不可用）：用于区分「任务 owner」与
// 「请求 owner」两条解析路径。
type perOwnerTenants map[string]*storage.Tenant

func (p perOwnerTenants) TenantFor(owner string) *storage.Tenant { return p[owner] }

// archiveEnv 是归档入口用例的共用装配：真实租户（cloud/t1/a.bin + cloud/t2/b.bin）、
// 一个已完成任务与一个进行中任务、一个包含两者的组。
type archiveEnv struct {
	svc     *Service
	tasks   *stubTasks
	groups  *stubGroups
	tenants perOwnerTenants
	quota   *stubArchiveQuota
	targets *stubArchiveTargets
	vols    *stubVolumes
	mem     *memFS
	root    *storage.Root
}

func newArchiveEnv(t *testing.T, opts ...func(*archiveEnv)) *archiveEnv {
	t.Helper()
	root := openTestRoot(t)
	tenant := openTestTenant(t, root, "alice")
	writeRootFile(t, tenant.Root(), "cloud/t1/a.bin", "aaaaa")
	writeRootFile(t, tenant.Root(), "cloud/t2/b.bin", "bbbbb")

	mem := newMemFS()
	env := &archiveEnv{
		tasks: &stubTasks{tasks: map[string]*cloud.CloudTask{
			"t1": {ID: "t1", Owner: "alice", Filename: "a.bin", Status: "completed"},
			"t2": {ID: "t2", Owner: "alice", Filename: "b.bin", Status: "downloading"},
		}},
		groups:  &stubGroups{groups: map[string]*cloud.CloudTaskGroup{"g1": {ID: "g1", Owner: "alice", TaskIDs: []string{"t1", "t2"}}}},
		tenants: perOwnerTenants{"alice": tenant},
		quota:   &stubArchiveQuota{settleActual: 1234},
		targets: &stubArchiveTargets{skipVerify: true},
		vols: &stubVolumes{
			allowed: map[string]bool{"alice\x00vault": true},
			fs:      mem,
			scheme:  "vault",
			vol:     volume.Volume{Name: "vault"},
		},
		mem:  mem,
		root: tenant.Root(),
	}
	for _, opt := range opts {
		opt(env)
	}
	env.svc = New(Deps{
		Tasks:   env.tasks,
		Groups:  env.groups,
		Tenants: env.tenants,
		Volumes: env.vols,
		Quota:   env.quota,
		Targets: env.targets,
		Audit:   &stubAudit{},
		Logger:  slog.New(slog.DiscardHandler),
	})
	return env
}

// archivePost 构造归档 POST 请求（handler 用 PathValue 取 id）。
func archivePost(t *testing.T, path, id, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	return req
}

// TestArchiveTask_SuccessByBucket 单任务归档落到租户 archive 桶：响应 checksum 必须等于
// 落盘产物的 SHA-256，产物可解压且条目名为任务文件名，占用登记与响应一致。
func TestArchiveTask_SuccessByBucket(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	rec := httptest.NewRecorder()

	env.svc.ArchiveTask(rec, archivePost(t, "/api/cloud/tasks/t1/archive", "t1", `{"archive_name":"t1-archive.tar.gz"}`), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeArchive(t, rec)
	raw := readRootFile(t, env.root, "archive/t1-archive.tar.gz")
	// size 取落盘实测（不是配额对账值），checksum 取流式哈希。
	if !got.Success || got.File != "t1-archive.tar.gz" || got.TaskCount != 1 || got.Size != int64(len(raw)) {
		t.Fatalf("响应 = %+v（size 期望落盘实测 %d）", got, len(raw))
	}
	sum := sha256.Sum256(raw)
	if got.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %q，期望落盘产物 SHA-256 %s", got.Checksum, hex.EncodeToString(sum[:]))
	}
	entries := gunzipTarEntries(t, raw)
	if entries["a.bin"] != "aaaaa" {
		t.Fatalf("归档条目 = %v，期望 a.bin=aaaaa", entries)
	}
	if env.svc.ArchiveUsage("alice", "t1-archive.tar.gz") != 1234 {
		t.Fatalf("占用登记 = %d，期望按对账实际 1234", env.svc.ArchiveUsage("alice", "t1-archive.tar.gz"))
	}
	if env.quota.released {
		t.Fatal("成功路径不得释放预留")
	}
}

// TestArchiveTask_DefaultNameAndRejections 单任务归档的拒绝矩阵（状态码/文案）。
func TestArchiveTask_DefaultNameAndRejections(t *testing.T) {
	t.Parallel()

	t.Run("default name is derived from task id", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{}`), "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := decodeArchive(t, rec); !strings.HasPrefix(got.File, "cloud-task-t1-") || !strings.HasSuffix(got.File, ".tar.gz") {
			t.Fatalf("默认归档名 = %q，期望 cloud-task-t1-<ts>.tar.gz", got.File)
		}
	})

	cases := []struct {
		name    string
		body    string
		taskID  string
		mutate  func(*archiveEnv)
		want    int
		wantMsg string
	}{
		{
			name: "task not completed", body: `{}`, taskID: "t2",
			want: http.StatusBadRequest, wantMsg: `task status is "downloading", expected "completed"`,
		},
		{name: "invalid body", body: `{`, taskID: "t1", want: http.StatusBadRequest, wantMsg: msgInvalidRequestBody},
		{
			name: "extras without target", body: `{"encrypt":true}`, taskID: "t1",
			want: http.StatusBadRequest,
		},
		{
			name: "source tenant unavailable", body: `{}`, taskID: "t1",
			mutate: func(e *archiveEnv) { delete(e.tenants, "alice") },
			want:   http.StatusBadRequest, wantMsg: msgArchiveStatFail,
		},
		{
			name: "invalid archive name", body: `{"archive_name":".."}`, taskID: "t1",
			want: http.StatusBadRequest, wantMsg: "invalid archive name",
		},
		{
			name: "archive output tenant unavailable", body: `{}`, taskID: "t1",
			// 任务 owner（alice）可解析源，请求 owner（bob）不可解析输出租户 ⇒ 500。
			mutate: func(e *archiveEnv) {},
			want:   http.StatusInternalServerError, wantMsg: msgArchiveDirFail,
		},
		{
			name: "reserve failed", body: `{}`, taskID: "t1",
			mutate: func(e *archiveEnv) {
				e.quota.reserveStatus = http.StatusInsufficientStorage
				e.quota.reserveMsg = "配额不足"
			},
			want: http.StatusInsufficientStorage, wantMsg: "配额不足",
		},
		{
			name: "settle failed", body: `{}`, taskID: "t1",
			mutate: func(e *archiveEnv) {
				e.quota.settleStatus = http.StatusInsufficientStorage
				e.quota.settleMsg = "对账失败"
			},
			want: http.StatusInsufficientStorage, wantMsg: "对账失败",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := newArchiveEnv(t)
			if c.mutate != nil {
				c.mutate(env)
			}
			owner := "alice"
			if c.name == "archive output tenant unavailable" {
				owner = "bob"
			}
			rec := httptest.NewRecorder()

			env.svc.ArchiveTask(rec, archivePost(t, "/", c.taskID, c.body), owner)

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.wantMsg != "" {
				if got := decodeArchive(t, rec).Message; got != c.wantMsg {
					t.Fatalf("message = %q，want %q", got, c.wantMsg)
				}
			}
		})
	}

	t.Run("invalid source path", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.tasks.tasks["t1"].Filename = "../escape.bin"
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{}`), "alice")

		if rec.Code != http.StatusBadRequest || decodeArchive(t, rec).Message != msgArchiveStatFail {
			t.Fatalf("status = %d body=%s（源路径穿越必须 fail-closed）", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing source file", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.tasks.tasks["t1"].Filename = "missing.bin"
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{}`), "alice")

		if rec.Code != http.StatusBadRequest || decodeArchive(t, rec).Message != msgArchiveStatFail {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("archive file already exists releases reservation", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive/dup.tar.gz", "existing")
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{"archive_name":"dup.tar.gz"}`), "alice")

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if !env.quota.released {
			t.Fatal("同名冲突必须释放已预留配额（防泄漏）")
		}
		// 既有产物不得被破坏。
		if raw := readRootFile(t, env.root, "archive/dup.tar.gz"); string(raw) != "existing" {
			t.Fatalf("既有归档被改写：%q", raw)
		}
	})

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.SetPathValue("id", "t1")
		req.Body = &jsonThenErrorBody{data: `{}`}

		env.svc.ArchiveTask(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestArchiveTaskToVolume_ACLAndOpenFailure 单任务转存路径：ACL 拒绝 403（且不打开源）、
// 归档名非法 400、源打开失败 500（stat 与 open 之间的竞态由直接调用覆盖）。
func TestArchiveTaskToVolume_ACLAndOpenFailure(t *testing.T) {
	t.Parallel()
	req := CloudArchiveRequest{TargetVolume: "vault", ArchiveName: "a.tar.gz"}

	t.Run("acl rejected", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.vols.allowed = map[string]bool{}
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{"target_volume":"vault"}`), "alice")

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid archive name", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{"target_volume":"vault","archive_name":".."}`), "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("source open failure", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		tenant := env.tenants["alice"]
		srcRel := "cloud/t1/a.bin"
		info, err := tenant.Root().Stat(srcRel)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if rErr := tenant.Root().Remove(srcRel); rErr != nil {
			t.Fatalf("remove: %v", rErr)
		}
		task := &cloud.CloudTask{ID: "t1", Owner: "alice", Filename: "a.bin", Status: "completed"}
		rec := httptest.NewRecorder()

		env.svc.archiveTaskToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), "alice", "t1", task,
			req, tenant, srcRel, info, req.ArchiveName)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(decodeArchive(t, rec).Message, "open source failed") {
			t.Fatalf("message = %q", decodeArchive(t, rec).Message)
		}
	})

	t.Run("success to volume", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveTask(rec, archivePost(t, "/", "t1", `{"target_volume":"vault","archive_name":"a","target_path":"dir/"}`), "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := decodeArchive(t, rec)
		if got.File != "vault://vault/alice/user/dir/a.tar.gz" {
			t.Fatalf("转存 URL = %q", got.File)
		}
		if _, ok := env.mem.data("alice/user/dir/a.tar.gz"); !ok {
			t.Fatal("目标卷未收到产物")
		}
	})
}

// TestArchiveBatch_SuccessAndSkips 批量归档：跳过未完成任务后打包成功，响应保留
// skipped 列表；全部跳过时的响应体已在 archive_test.go 锁定。
func TestArchiveBatch_SuccessAndSkips(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	rec := httptest.NewRecorder()

	env.svc.ArchiveBatch(rec, archivePost(t, "/api/cloud/archive", "", `{"task_ids":["t1","t2"],"archive_name":"batch.tar.gz"}`), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeArchive(t, rec)
	if !got.Success || got.File != "batch.tar.gz" || got.TaskCount != 1 || got.SkippedCount != 1 {
		t.Fatalf("响应 = %+v", got)
	}
	if len(got.SkippedTasks) != 1 || got.SkippedTasks[0] != "t2" {
		t.Fatalf("skipped = %v", got.SkippedTasks)
	}
	raw := readRootFile(t, env.root, "archive/batch.tar.gz")
	sum := sha256.Sum256(raw)
	if got.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("批量 checksum 与落盘产物不一致")
	}
}

// TestArchiveBatch_Rejections 批量归档的拒绝矩阵。
func TestArchiveBatch_Rejections(t *testing.T) {
	t.Parallel()
	tooMany := `{"task_ids":[` + strings.TrimSuffix(strings.Repeat(`"t1",`, 101), ",") + `]}`
	cases := []struct {
		name    string
		body    string
		mutate  func(*archiveEnv)
		want    int
		wantMsg string
	}{
		{name: "invalid body", body: `{`, want: http.StatusBadRequest, wantMsg: msgInvalidRequestBody},
		{name: "missing task ids", body: `{}`, want: http.StatusBadRequest, wantMsg: "task_ids is required"},
		{name: "too many task ids", body: tooMany, want: http.StatusBadRequest, wantMsg: "maximum 100 task IDs per request"},
		{name: "extras without target", body: `{"task_ids":["t1"],"compression":"zstd"}`, want: http.StatusBadRequest},
		{
			name: "reserve failed", body: `{"task_ids":["t1"]}`,
			mutate: func(e *archiveEnv) {
				e.quota.reserveStatus = http.StatusInsufficientStorage
				e.quota.reserveMsg = "配额不足"
			},
			want: http.StatusInsufficientStorage, wantMsg: "配额不足",
		},
		{
			name: "settle failed", body: `{"task_ids":["t1"]}`,
			mutate: func(e *archiveEnv) {
				e.quota.settleStatus = http.StatusInsufficientStorage
				e.quota.settleMsg = "对账失败"
			},
			want: http.StatusInsufficientStorage, wantMsg: "对账失败",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := newArchiveEnv(t)
			if c.mutate != nil {
				c.mutate(env)
			}
			rec := httptest.NewRecorder()

			env.svc.ArchiveBatch(rec, archivePost(t, "/", "", c.body), "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.wantMsg != "" {
				if got := decodeArchive(t, rec).Message; got != c.wantMsg {
					t.Fatalf("message = %q，want %q", got, c.wantMsg)
				}
			}
		})
	}

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Body = &jsonThenErrorBody{data: `{"task_ids":["t1"]}`}

		env.svc.ArchiveBatch(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("archive file already exists releases reservation", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive/dup.tar.gz", "existing")
		rec := httptest.NewRecorder()

		env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"],"archive_name":"dup.tar.gz"}`), "alice")

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if !env.quota.released {
			t.Fatal("同名冲突必须释放预留")
		}
	})
}

// TestArchiveBatchToVolume_Paths 批量转存：ACL 拒绝 403、归档名非法 400（转存路径独占
// 归一，与桶路径分离）、源打开失败 500、成功 200。
func TestArchiveBatchToVolume_Paths(t *testing.T) {
	t.Parallel()

	t.Run("acl rejected", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.vols.allowed = map[string]bool{}
		rec := httptest.NewRecorder()

		env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"],"target_volume":"vault"}`), "alice")

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid archive name", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"],"target_volume":"vault","archive_name":".."}`), "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("source open failure", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		sources := []ArchiveSource{{Root: env.root, Rel: "cloud/t1/missing.bin", TarRel: "t1/missing.bin"}}
		rec := httptest.NewRecorder()

		env.svc.archiveBatchToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), "alice",
			CloudArchiveBatchRequest{TargetVolume: "vault", ArchiveName: "b.tar.gz"}, sources, 1, "b.tar.gz")

		if rec.Code != http.StatusInternalServerError || !strings.Contains(decodeArchive(t, rec).Message, "open source failed") {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("success to volume", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveBatch(rec, archivePost(t, "/", "", `{"task_ids":["t1"],"target_volume":"vault","archive_name":"b"}`), "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if _, ok := env.mem.data("alice/user/archive/b.tar.gz"); !ok {
			t.Fatal("目标卷未收到批量归档产物")
		}
	})
}

// TestArchiveGroup_BranchMatrix 组归档分支：请求解析、名称归一、可选文件收集、
// 目标目录准备、对账失败、同名冲突（含 O_EXCL 直接调用）。
func TestArchiveGroup_BranchMatrix(t *testing.T) {
	t.Parallel()

	t.Run("default name", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := decodeArchive(t, rec).File; !strings.HasPrefix(got, "g1-") {
			t.Fatalf("默认组归档名 = %q，期望 g1-<ts>.tar.gz", got)
		}
	})

	t.Run("group not found", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "ghost", `{}`), "alice")

		if rec.Code != http.StatusNotFound || decodeArchive(t, rec).Message != msgGroupNotFound {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{`), "alice")

		if rec.Code != http.StatusBadRequest || decodeArchive(t, rec).Message != msgInvalidRequestBody {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid archive name", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{"archive_name":".."}`), "alice")

		if rec.Code != http.StatusBadRequest || decodeArchive(t, rec).Message != "invalid archive name" {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no completed files", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.groups.groups["g1"].TaskIDs = []string{"t2"}
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		got := decodeArchive(t, rec)
		if got.Message != "no completed files to archive in group" || got.SkippedCount != 1 {
			t.Fatalf("响应 = %+v", got)
		}
	})

	t.Run("output tenant unavailable", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		delete(env.tenants, "alice")
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		// 收集阶段先失败（任务 owner 租户不可用）⇒ 全部跳过 ⇒ 400。
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("settle failed", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.quota.settleStatus = http.StatusInsufficientStorage
		env.quota.settleMsg = "对账失败"
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		if rec.Code != http.StatusInsufficientStorage || decodeArchive(t, rec).Message != "对账失败" {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("reserve failed", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		env.quota.reserveStatus = http.StatusInsufficientStorage
		env.quota.reserveMsg = "配额不足"
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{}`), "alice")

		if rec.Code != http.StatusInsufficientStorage || decodeArchive(t, rec).Message != "配额不足" {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("existing archive name conflict", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		writeRootFile(t, env.root, "archive/exists.tar.gz", "existing")
		rec := httptest.NewRecorder()

		env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{"archive_name":"exists.tar.gz"}`), "alice")

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("o_excl conflict releases reservation", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		// 默认名带时间戳 ⇒ 绕过 PrepTarget 的同名预检，直接命中 O_EXCL。
		rel := "archive/race.tar.gz"
		writeRootFile(t, env.root, rel, "existing")
		rec := httptest.NewRecorder()

		checksum, ok := env.svc.archiveGroupCreateTar(rec, nil, env.root, rel, nil, 10, "g1")
		if ok || checksum != "" {
			t.Fatalf("O_EXCL 冲突应失败，got (%q,%v)", checksum, ok)
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if !env.quota.released {
			t.Fatal("O_EXCL 冲突必须释放预留")
		}
	})

	t.Run("tar creation failure cleans partial output", func(t *testing.T) {
		t.Parallel()
		env := newArchiveEnv(t)
		rec := httptest.NewRecorder()
		rel := "archive/broken.tar.gz"
		sources := []ArchiveSource{{Root: env.root, Rel: "cloud/t1/missing.bin", TarRel: "t1/missing.bin"}}

		if _, ok := env.svc.archiveGroupCreateTar(rec, sources, env.root, rel, nil, 10, "g1"); ok {
			t.Fatal("源缺失应失败")
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if _, err := env.root.Stat(rel); err == nil {
			t.Fatal("失败必须清理半成品归档")
		}
		if !env.quota.released {
			t.Fatal("打包失败必须释放预留")
		}
	})
}

// TestArchiveGroupToVolume_RegistersURL 组归档转存成功后必须把**转存 URL** 落库到组
// （供组删除联动清理），响应含 URL 与条目数。
func TestArchiveGroupToVolume_RegistersURL(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	rec := httptest.NewRecorder()

	env.svc.ArchiveGroup(rec, archivePost(t, "/", "g1", `{"target_volume":"vault","archive_name":"g"}`), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	want := "vault://vault/alice/user/archive/g.tar.gz"
	if got := decodeArchive(t, rec); got.File != want || got.TaskCount != 1 {
		t.Fatalf("响应 = %+v，期望 URL=%q 且 1 个条目", got, want)
	}
	if env.groups.setArchiveID != "g1" || env.groups.setArchiveFile != want {
		t.Fatalf("组归档落库 = (%q,%q)，期望 (g1,%q)", env.groups.setArchiveID, env.groups.setArchiveFile, want)
	}
}

// TestArchiveGroupCollectFiles_SkipReasons 组内逐条跳过原因：任务不存在、租户不可用、
// 路径非法、文件缺失——全部计入 skipped 而非中断（组归档必须尽量产出）。
func TestArchiveGroupCollectFiles_SkipReasons(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	env.groups.groups["g1"].TaskIDs = []string{"missing-task", "t-bad-path", "t-ghost-tenant", "t-missing-file", "t1"}
	env.tasks.tasks["t-bad-path"] = &cloud.CloudTask{ID: "t-bad-path", Owner: "alice", Filename: "../x", Status: "completed"}
	env.tasks.tasks["t-ghost-tenant"] = &cloud.CloudTask{ID: "t-ghost-tenant", Owner: "ghost", Filename: "g.bin", Status: "completed"}
	env.tasks.tasks["t-missing-file"] = &cloud.CloudTask{ID: "t-missing-file", Owner: "alice", Filename: "nope.bin", Status: "completed"}
	rec := httptest.NewRecorder()

	sources, skipped, total, ok := env.svc.archiveGroupCollectFiles(rec, "g1", "alice")

	if !ok || len(sources) != 1 || sources[0].TarRel != "t1/a.bin" {
		t.Fatalf("收集结果 = %+v（ok=%v）", sources, ok)
	}
	if len(skipped) != 4 || total != 5 {
		t.Fatalf("skipped = %v（want 4 项），total = %d（want 5）", skipped, total)
	}
}

// TestArchiveTaskSource_SkipReasons 批量归档逐任务解析的跳过原因覆盖（与组归档同源规则）。
func TestArchiveTaskSource_SkipReasons(t *testing.T) {
	t.Parallel()
	env := newArchiveEnv(t)
	env.tasks.tasks["t-bad-path"] = &cloud.CloudTask{ID: "t-bad-path", Owner: "alice", Filename: "../x", Status: "completed"}
	env.tasks.tasks["t-ghost-tenant"] = &cloud.CloudTask{ID: "t-ghost-tenant", Owner: "ghost", Filename: "g.bin", Status: "completed"}
	env.tasks.tasks["t-missing-file"] = &cloud.CloudTask{ID: "t-missing-file", Owner: "alice", Filename: "nope.bin", Status: "completed"}

	for _, id := range []string{"missing-task", "t2", "t-bad-path", "t-ghost-tenant", "t-missing-file"} {
		if _, _, skip := env.svc.archiveTaskSource("alice", id); !skip {
			t.Errorf("任务 %q 应被跳过", id)
		}
	}
	if src, size, skip := env.svc.archiveTaskSource("alice", "t1"); skip || size != 5 || src.TarRel != "t1/a.bin" {
		t.Fatalf("有效任务解析 = (%+v, %d, skip=%v)", src, size, skip)
	}
}

// TestWriteJSON_MarshalFailureFallsBack500 响应序列化失败必须 500 + 通用错误包
// （不得写出空 body 或部分 JSON）。
func TestWriteJSON_MarshalFailureFallsBack500(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Logger: slog.New(slog.DiscardHandler)})
	rec := httptest.NewRecorder()

	svc.writeJSON(rec, make(chan int), http.StatusOK)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get(headerContentType); got != contentTypeJSON {
		t.Fatalf("Content-Type = %q，期望 JSON（头必须先设）", got)
	}
}

// TestCreateTarGz_ErrorBranches tar 打包助手的错误面：源缺失（清理半成品）、
// 输出父路径是文件（创建失败）、已存在（errArchiveExists）。
func TestCreateTarGz_ErrorBranches(t *testing.T) {
	t.Parallel()
	root := openTestRoot(t)
	writeRootFile(t, root, "src.bin", "abc")
	writeRootFile(t, root, "blocker", "file")
	logger := slog.New(slog.DiscardHandler)

	if _, err := createTarGz(root, "missing.bin", "missing.bin", root, "out/missing.tar.gz", logger); err == nil {
		t.Fatal("源缺失应失败")
	} else if _, statErr := root.Stat("out/missing.tar.gz"); statErr == nil {
		t.Fatal("打包失败必须清理输出文件")
	}

	if _, err := createTarGz(root, "src.bin", "src.bin", root, "blocker/x.tar.gz", logger); err == nil {
		t.Fatal("父路径是文件时应创建失败")
	} else if !strings.Contains(err.Error(), "create archive output") {
		t.Fatalf("error = %v，期望包装创建失败", err)
	}

	if _, err := createTarGz(root, "src.bin", "src.bin", root, "dup.tar.gz", logger); err != nil {
		t.Fatalf("首次打包应成功：%v", err)
	}
	if _, err := createTarGz(root, "src.bin", "src.bin", root, "dup.tar.gz", logger); !errors.Is(err, errArchiveExists) {
		t.Fatalf("已存在应返回 errArchiveExists，got %v", err)
	}
}

// TestCreateMultiFileTarGz_ErrorBranches 多文件打包助手的错误面（与单文件同纪律）。
func TestCreateMultiFileTarGz_ErrorBranches(t *testing.T) {
	t.Parallel()
	root := openTestRoot(t)
	writeRootFile(t, root, "a.bin", "a")
	writeRootFile(t, root, "blocker", "file")
	logger := slog.New(slog.DiscardHandler)

	if _, err := createMultiFileTarGz([]ArchiveSource{{Root: root, Rel: "missing.bin", TarRel: "m"}}, root, "out/multi.tar.gz", logger); err == nil {
		t.Fatal("源缺失应失败")
	} else if _, statErr := root.Stat("out/multi.tar.gz"); statErr == nil {
		t.Fatal("失败必须清理输出")
	}

	if _, err := createMultiFileTarGz([]ArchiveSource{{Root: root, Rel: "a.bin", TarRel: "a"}}, root, "blocker/y.tar.gz", logger); err == nil {
		t.Fatal("父路径是文件时应失败")
	}

	sources := []ArchiveSource{{Root: root, Rel: "a.bin", TarRel: "a"}}
	if _, err := createMultiFileTarGz(sources, root, "dup-multi.tar.gz", logger); err != nil {
		t.Fatalf("首次打包应成功：%v", err)
	}
	if _, err := createMultiFileTarGz(sources, root, "dup-multi.tar.gz", logger); !errors.Is(err, errArchiveExists) {
		t.Fatalf("已存在应返回 errArchiveExists，got %v", err)
	}
}

// gunzipTarEntries 解压 tar.gz 并映射条目名 → 内容（验证归档完整可读）。
func gunzipTarEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip 头解析失败: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	entries := map[string]string{}
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar 解析失败: %v", nextErr)
		}
		body, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatalf("读条目 %q: %v", hdr.Name, readErr)
		}
		entries[hdr.Name] = string(body)
	}
	return entries
}

// 编译期锚点：归档用例依赖的替身仍满足接口（接口加方法时先红）。
var (
	_ Tenants       = perOwnerTenants{}
	_ io.ReadCloser = (*jsonThenErrorBody)(nil)
	_ syncpkg.FS    = (*memFS)(nil)
	_               = os.FileMode(0)
	_               = filepath.ToSlash
)
