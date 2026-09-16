// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_init_state_publish_test.go 钉住 init 阶段「会话状态发布」的并发契约：
// 定卷/预留/在途临时名（Volume/Reservation/Pool/PoolRes/StorageMgrReserved/TempPath）由 init
// 写回**store 持有的会话对象**，而同一会话会被并发请求经生产读路径整结构深拷贝
// （GetSession → copySession；PersistNow/persistSession 同形）。因此这些字段的写入必须与读者
// 在同一把 store 锁下，否则 `-race` 下即为数据竞争（审计 C-8：`string` 头/指针的 torn read）。

import (
	"net/http"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"
	"time"
)

// TestService_UploadInit_PublishesSessionStateUnderStoreLock 用真实 init 处理器 + 并发读者
// 复现审计 C-8：读者持续经 GetSession 深拷贝会话，写者执行 init（定卷 → 预留 → 临时名三组
// 字段发布）。修复前 init 直接改返回值上的字段（不在 store 锁内）⇒ 与读者构成无序访问对，
// `-race` 必红；修复后发布走锁内 setter ⇒ 有序、无竞争。
func TestService_UploadInit_PublishesSessionStateUnderStoreLock(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	// 注入容量回退替身 ⇒ init 走 P5 预留支（第 6 处字段发布：StorageMgrReserved）。
	env.capacity = &fakeCapacity{}
	h := env.handlers(4)

	const uploadID = "publish-race"
	content := []byte("0123456789")

	// 读者：只走生产读路径（GetSession 内部持 RLock + 整结构深拷贝），不碰内部字段。
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = env.us.GetSession(uploadID)
				goruntime.Gosched()
			}
		})
	}

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "dir/publish.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": sha256Hex(content), "file_mod_time": 0,
	})
	close(stop)
	readers.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	// 行为面（发布必须对后续读者可见）：在途临时名已写回 store 持有的会话对象。
	sess := env.us.GetSession(uploadID)
	if sess == nil || sess.TempPath == "" {
		t.Fatalf("init 后 TempPath 应已发布到会话: %+v", sess)
	}
	if sess.StorageMgrReserved != int64(len(content)) {
		t.Fatalf("P5 回退预留登记应已发布: got %d want %d", sess.StorageMgrReserved, len(content))
	}
}

// TestUploadStore_SetSessionInitFields_PublishesToReaders 覆盖三个 init 发布 setter 的行为面：
// 都写回 store 持有的会话（对后续读者可见）；未知 upload_id （会话已被并发删除）返回 false，
// 调用方无需特殊处理。
func TestUploadStore_SetSessionInitFields_PublishesToReaders(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	us := MustNewUploadStore(filepath.Join(t.TempDir(), "chunk"), time.Hour, nil)
	defer us.Stop()
	if _, err := us.CreateSession("pub-sid", "f.txt", 8, 4, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if !us.SetSessionRoute("pub-sid", "disk2", nil, nil, nil) {
		t.Fatal("SetSessionRoute 命中会话应返回 true")
	}
	if !us.SetSessionStorageMgrReserved("pub-sid", 8) {
		t.Fatal("SetSessionStorageMgrReserved 命中会话应返回 true")
	}
	if !us.SetSessionTempPath("pub-sid", "user/f.txt") {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}
	sess := us.GetSession("pub-sid")
	if sess == nil || sess.Volume != "disk2" || sess.StorageMgrReserved != 8 || sess.TempPath != "user/f.txt" {
		t.Fatalf("setter 未发布到 store 持有的会话: %+v", sess)
	}

	if us.SetSessionRoute("missing", "disk2", nil, nil, nil) {
		t.Fatal("SetSessionRoute 未知 upload_id 应返回 false")
	}
	if us.SetSessionStorageMgrReserved("missing", 8) {
		t.Fatal("SetSessionStorageMgrReserved 未知 upload_id 应返回 false")
	}
	if us.SetSessionTempPath("missing", "user/f.txt") {
		t.Fatal("SetSessionTempPath 未知 upload_id 应返回 false")
	}
}
