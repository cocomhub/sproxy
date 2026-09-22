// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// uploadTierFile 经 HTTP 上传文件到指定卷（返回 filename 相对路径）。
func uploadTierFile(t *testing.T, baseURL, volName, name, content string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, werr := fw.Write([]byte(content)); werr != nil {
		t.Fatalf("write form: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	url := baseURL + "/upload?volume=" + volName
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex([]byte(content)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload %s 失败: %d %s", name, resp.StatusCode, string(body))
	}
}

// tierVolsCfg 构造 hot+cold 双卷配置（default=hot，cold1=cold）。
func tierVolsCfg(tmpDir string) *Config {
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.Volumes = []VolumeConfig{
		{Name: "default", Root: filepath.Join(tmpDir, "hot"), Tier: "hot"},
		{Name: "cold1", Root: filepath.Join(tmpDir, "cold"), Tier: "cold"},
	}
	return cfg
}

// TestTier_DowngradeMovesOldBigFileToCold 自动降级：hot 卷旧大文件 → 迁移到 cold 卷。
func TestTier_DowngradeMovesOldBigFileToCold(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierVolsCfg(t.TempDir())
		cfg.TierPolicy = TierPolicyConfig{
			Interval:   time.Hour, // 手动触发 scanOnce 测试（不依赖真实 ticker）
			MaxAgeHot:  time.Minute,
			MinSizeHot: 10, // 10 字节以上且 mtime 超 1 分钟 → 降级
		}
	})

	uploadTierFile(t, baseURL, "default", "old.txt", strings.Repeat("x", 100))

	// 回拨 mtime 使其超过 MaxAgeHot。
	h := registeredHandlersFrom(t, cfgPtr)
	oldRel := "old.txt"
	rt := h.tenantFor("")
	root := rt.Root()
	abs, ok := root.Abs("user/" + oldRel)
	if !ok {
		t.Fatalf("Abs 解析失败")
	}
	if err := os.Chtimes(abs, time.Now().Add(-2*time.Minute), time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// 手动执行一次降级扫描（等 ticker 不可靠）。
	tm := newTierManager(h, time.Hour)
	tm.scanOnce()

	// 断言：文件已迁到 cold 卷（cold 卷租户 user 桶存在该文件）。
	coldTnt := h.volumeTenant("cold1", "")
	if coldTnt == nil || coldTnt.Root() == nil {
		t.Fatalf("cold 卷租户不可用")
	}
	if _, err := coldTnt.Root().Stat("user/" + oldRel); err != nil {
		t.Fatalf("降级后 cold 卷应存在 old.txt，stat err: %v", err)
	}
	// hot 卷应不再有该文件。
	hotTnt := h.volumeTenant("default", "")
	if _, err := hotTnt.Root().Stat("user/" + oldRel); err == nil {
		t.Fatalf("降级后 hot 卷不应再有 old.txt")
	}
}

// TestTier_DowngradeSkipsYoungSmallFiles 自动降级条件：不够旧/不够大的文件不迁移。
func TestTier_DowngradeSkipsYoungSmallFiles(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierVolsCfg(t.TempDir())
		cfg.TierPolicy = TierPolicyConfig{
			Interval:   time.Hour,
			MaxAgeHot:  time.Minute,
			MinSizeHot: 100,
		}
	})
	uploadTierFile(t, baseURL, "default", "small.txt", "tiny")                      // 5 字节 < MinSizeHot
	uploadTierFile(t, baseURL, "default", "bigyoung.txt", strings.Repeat("z", 500)) // 够大但刚写（年轻）

	h := registeredHandlersFrom(t, cfgPtr)
	tm := newTierManager(h, time.Hour)
	tm.scanOnce()

	// small.txt（5 字节 < MinSizeHot 100）不应迁移。
	hotTnt := h.volumeTenant("default", "")
	if _, err := hotTnt.Root().Stat("user/small.txt"); err != nil {
		t.Fatalf("小于 MinSizeHot 的文件不应降级，stat err: %v", err)
	}
	// bigyoung.txt（够大但 mtime 新 < MaxAgeHot）不应迁移（年龄条件守卫）。
	if _, err := hotTnt.Root().Stat("user/bigyoung.txt"); err != nil {
		t.Fatalf("够大但年轻的文件不应降级，stat err: %v", err)
	}
}

// TestTier_PromoteOnRead 读时回迁：访问 cold 卷文件 → 自动移回 hot 卷（API 无感）。
func TestTier_PromoteOnRead(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierVolsCfg(t.TempDir())
	})
	// 直接写文件到 cold 卷（模拟已降级）。
	uploadTierFile(t, baseURL, "cold1", "cold.txt", "cold-content")

	// 下载该文件（无 ?volume=，跨卷定位命中 cold1）→ 触发回迁。
	resp, err := http.DefaultClient.Get(baseURL + "/download?filename=cold.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download 应 200，got %d", resp.StatusCode)
	}

	// 断言：文件已回迁到 hot（default）卷。
	h := registeredHandlersFrom(t, cfgPtr)
	hotTnt := h.volumeTenant("default", "")
	if _, err := hotTnt.Root().Stat("user/cold.txt"); err != nil {
		t.Fatalf("回迁后 hot 卷应存在 cold.txt，stat err: %v", err)
	}
	coldTnt := h.volumeTenant("cold1", "")
	if _, err := coldTnt.Root().Stat("user/cold.txt"); err == nil {
		t.Fatalf("回迁后 cold 卷不应再有 cold.txt")
	}
}

// TestTier_DefaultOff 默认关零回归：无 tier_policy 配置不自动降级。
func TestTier_DefaultOff(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.StorageRoot = t.TempDir()
		// 不配 tier_policy：Interval=0
	})
	uploadTierFile(t, baseURL, "default", "keep.txt", strings.Repeat("y", 50))

	h := registeredHandlersFrom(t, cfgPtr)
	if h.cfgPtr.Load().TierPolicy.Interval != 0 {
		t.Fatalf("缺省 tier_policy.interval 应为 0（默认关）")
	}
	// 无 ticker 启动（tierStop nil）。
	if h.tierStop != nil {
		t.Fatalf("默认关时 tierStop 不应挂载")
	}
}
