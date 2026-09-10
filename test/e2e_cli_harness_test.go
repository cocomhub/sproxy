// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// cliEnv 打包真二进制 CLI e2e 环境：真实 sproxy 子进程 + sclient 二进制。
//
// 设计要点（PR-F3）：
//   - 服务端是真实 sproxy 二进制子进程（startSPROXYImpl 构建 + 启动 + 健康等待）；
//   - 客户端是真实 sclient 二进制子进程（e2eBinPath 整包只构建一次）；
//   - 所有断言必须落在「真实副作用」上：磁盘文件（findFilesNamed + 内容/checksum）
//     或签名 HTTP API 响应（authedHTTPClient），不得只断言退出码/stdout 字样。
type cliEnv struct {
	BaseURL     string // http://127.0.0.1:<port>
	StorageRoot string // 默认卷根（<tmp>/uploads）：磁盘副作用断言根
	TmpDir      string // 测试临时目录（sproxy.yaml / 本地文件 / 输出文件）
	sclientBin  string // e2eBinPath(t, "cmd/sclient")
}

// startCLIEnv 启动真实 sproxy（复用 startSPROXYImpl，含 e2eTestAK/SK 凭据种子），
// 注册 t.Cleanup 关停；extraConfig 追加到 sproxy 临时配置（同 startSPROXYImpl 语义）。
func startCLIEnv(t *testing.T, extraConfig string) *cliEnv {
	t.Helper()
	baseURL, storageRoot, cleanup := startSPROXYImpl(t, extraConfig)
	t.Cleanup(cleanup)
	return &cliEnv{
		BaseURL:     baseURL,
		StorageRoot: storageRoot,
		TmpDir:      filepath.Dir(storageRoot),
		sclientBin:  e2eBinPath(t, "cmd/sclient"),
	}
}

// cliPrefixFlags 返回 sclient 前置全局 flag（顺序固定，置于子命令前）。
//
// --config 指向**不存在**的路径：sclientcfg.New 对 IsNotExist 静默忽略，从而隔离本机
// ~/.config/sproxy/sclient.yaml（--server 挡不住用户配置里的 access_key 等）。
// --access-key 非空即令 clientfactory 走 client.WithTunnel(ak, sk) → 加密隧道 localMux
// 路径，故目标 handler 必须双侧注册（本套件覆盖的路由均已双侧注册）。
func (e *cliEnv) cliPrefixFlags() []string {
	return []string{
		"--config", filepath.Join(e.TmpDir, "sclient.yaml"),
		"--server", e.BaseURL,
		"--access-key", e2eTestAK,
		"--access-key-secret", e2eTestSK,
		"--access-key-id", e2eTestID,
	}
}

// cliEnv 的 XDG 隔离目录名（每次 sclientRun 惰性创建，避免 --config 之外的
// 用户态渗入：sclient 的 cd 当前目录持久化在 XDG **缓存**目录，与 --config 无关，
// 若本机存在 cd 状态会改变相对路径解析 → list/name 断言失败）。
const (
	cliXDGCacheDir  = "xdg-cache"
	cliXDGConfigDir = "xdg-config"
)

// sclientRun 以子进程运行 sclient，注入隔离配置 + SproxySig 凭据与全局 flag。
//
// 隔离三件套（memory: cli-test-config-isolation）：
//   - --config 指向不存在路径 → sclientcfg.New 静默忽略，不读本机 sclient.yaml；
//   - XDG_CACHE_HOME / XDG_CONFIG_HOME 指向本次测试临时目录 → cd 持久化的 currentDir
//     与 identity.json 均不会读到本机用户状态（跨平台：adrg/xdg 在 Windows 亦优先取
//     这两个环境变量）。
//
// 返回 stdout/stderr/err；调用方自行判定（负例用 raw，正例用 sclient）。
func (e *cliEnv) sclientRun(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	full := append(e.cliPrefixFlags(), args...)
	cmd := exec.Command(e.sclientBin, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(e.TmpDir, cliXDGCacheDir),
		"XDG_CONFIG_HOME="+filepath.Join(e.TmpDir, cliXDGConfigDir),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// sclient 运行 sclient，非零退出即 t.Fatalf（附 stdout/stderr），返回 stdout。
func (e *cliEnv) sclient(t *testing.T, dir string, args ...string) string {
	t.Helper()
	stdout, stderr, err := e.sclientRun(t, dir, args...)
	if err != nil {
		t.Fatalf("sclient %v 失败: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout, stderr)
	}
	return stdout
}

// sclientJSON 运行 sclient（自动追加 --json）并把 stdout 解析进 v。
// 缺省非 0 退出即 Fatalf；v 用带 int64 字段的类型（version_id / size）避免精度丢失。
func (e *cliEnv) sclientJSON(t *testing.T, dir string, v any, args ...string) {
	t.Helper()
	full := append([]string{"--json"}, args...)
	stdout := e.sclient(t, dir, full...)
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatalf("sclient %v --json 输出解析失败: %v\nstdout:\n%s", args, err, stdout)
	}
}

// findFilesNamed 在 root 下递归找 basename == name 的条目，返回绝对路径切片（按路径排序）。
// 用于磁盘副作用断言（不硬编码 tenant 段——tenant 名由凭据 Owner 推导，属实现细节）。
// root 不存在时不报错（返回空切片），便于断言「某卷下无该文件」。
func findFilesNamed(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return nil
			}
			return werr
		}
		if d.Name() == name {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir %s: %v", root, err)
	}
	sort.Strings(found)
	return found
}

// rawHTTPClient 是公开路由探测用的短超时客户端（/s/{token} 无签名，裸 GET 即可）。
var rawHTTPClient = &http.Client{Timeout: 30 * time.Second}

// rawGET 对 url 发起无签名 GET（用于公开 /s/{token} 路由），返回 status/headers/body。
func rawGET(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := rawHTTPClient.Get(url)
	if err != nil {
		t.Fatalf("rawGET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, cerr := buf.ReadFrom(resp.Body); cerr != nil {
		t.Fatalf("rawGET %s 读响应体失败: %v", url, cerr)
	}
	return resp.StatusCode, resp.Header, buf.Bytes()
}
