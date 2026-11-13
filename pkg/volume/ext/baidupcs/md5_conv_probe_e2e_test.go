// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package baidupcs

// md5_conv_probe_e2e_test.go —— 【行为探针】量化百度网盘「上传后列表可见性 / md5 收敛」，
// 并检验两个假设：
//
//	H1「写入落节点 X、列举读打到节点 Y（跨节点复制延迟）」——若是，则**不同列举会命中不同 IP**，
//	   且"读到的那个节点的视图"决定可见性。
//	H2「**连接复用**决定可见性」——同一客户端保持 keep-alive（固定同一节点/会话）时，
//	   是否比"每次新建连接"更快看到刚上传的对象。
//
// 这是**受控对照**：同进程、同凭据、同 API（都是库的 `FilesDirectoriesList`），
// 唯一变量是**连接策略**（A=keep-alive 复用；B=DisableKeepAlives 每次新连接）。
// 另外并行用 CLI `ls -l`（子进程、独立连接）作为第三方意见。
//
// 判定口径（用户要求「必须用 ls -l 确认」）：收敛 ⇔ `ls -l` 有该条目 **且不含「可能不正确」**
// （CLI 侧规则见 `internal/pcscommand/ls_search.go:102`：`len(BlockList) > 1`）。
// ⚠️ 不用库的 `BlockList` 判定——实测其默认可能为空（`len==0` 恒成立 ⇒ 假绿）。
//
// 上传走 **CLI**：`uploadViaMultiUploader` 是本仓**已知慢路径**（「库上传 90s」，已 fail-closed 默认禁用），
// 生产走 binary/CLI 优先（8MiB 实测 ~3s）。**收敛是服务端行为**，与上传载体无关。
//
// 前置：`BAIDUPCS_E2E_ROOT`（真实网盘隔离根，运行期传入）+ `BDUSS` 环境变量（或 WSL 已登录）。
// 运行：
//
//	cd pkg/volume/ext/baidupcs && BAIDUPCS_E2E_ROOT='<隔离根>' \
//	  GOTOOLCHAIN=go1.27.1 GOWORK=off go test -tags=e2e -run TestE2E_MD5ConvergenceProbe -count=1 -v -timeout 20m
//
// 可选环境变量：MD5PROBE_SIZE（默认 8MiB）、MD5PROBE_WAIT（每阶段上限，默认 60s）。
import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
)

// ipHit 是一次出站请求的打点（含真实命中的服务端 IP）。
type ipHit struct {
	Host string
	IP   string
}

// ipTracer 包装底层 RoundTripper，记录每次请求命中的服务端 IP。
type ipTracer struct {
	inner http.RoundTripper
	mu    sync.Mutex
	hits  []ipHit
}

func (t *ipTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	var ip string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil {
				ip = info.Conn.RemoteAddr().String()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.inner.RoundTrip(req)
	t.mu.Lock()
	t.hits = append(t.hits, ipHit{Host: req.URL.Host, IP: ip})
	t.mu.Unlock()
	return resp, err
}

func (t *ipTracer) drain() []ipHit {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.hits
	t.hits = nil
	return out
}

// hostIPsOf 汇总"host(ip)"去重串（判定到底打到哪些**接口域名**）。
func hostIPsOf(hits []ipHit) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hits {
		k := h.Host + "(" + h.IP + ")"
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// ipsOf 汇总去重 IP（保持出现顺序）。
func ipsOf(hits []ipHit) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hits {
		if h.IP == "" || seen[h.IP] {
			continue
		}
		seen[h.IP] = true
		out = append(out, h.IP)
	}
	return out
}

// probeListVia 用指定库客户端做一次父目录列举，返回是否可见 + md5 + fsID。
// opts 为 nil 与 DefaultOrderOptions 分别测——CLI 用的是后者，参数不同可能走服务端不同路径。
func probeListVia(pcs *bdlib.BaiduPCS, dir, name string, opts *bdlib.OrderOptions) (visible bool, md5, fsID string) {
	fdl, pcsErr := pcs.FilesDirectoriesList(dir, opts)
	if pcsErr != nil {
		return false, "", ""
	}
	for _, fd := range fdl {
		if fd == nil || path.Base(strings.TrimSuffix(fd.Path, "/")) != name {
			continue
		}
		return true, fd.MD5, strconv.FormatInt(fd.FsID, 10)
	}
	return false, "", ""
}

// probeCLIVerdict 用 CLI `ls -l` 取权威判定：是否可见 + 是否已收敛（不含「可能不正确」）。
func probeCLIVerdict(bin, dir, name string) (visible, converged bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "ls", "-l", dir).CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.Contains(ln, name) {
			continue
		}
		return true, !strings.Contains(ln, "可能不正确")
	}
	return false, false
}

// probeMkRandFile 生成 n 字节**全新随机内容**（保证首次上传是真实上传，而非秒传 no-op）。
func probeMkRandFile(p string, n int) error {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func probeEnvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func probeEnvDur(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// probeSetupClients 建两个库客户端：A=keep-alive 复用（固定节点），B=每次新建连接。
// 两者都注入 IP 打点。唯一变量是连接策略 ⇒ 可干净地回答「复用连接是否更快可见」。
func probeSetupClients(t *testing.T, bduss string) (pcsA, pcsB *bdlib.BaiduPCS, trA, trB *ipTracer) {
	t.Helper()
	mk := func(keepAlive bool) (*bdlib.BaiduPCS, *ipTracer) {
		cli, err := NewClient(bduss, "")
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		pcs := cli.PCS()
		hc := pcs.GetClient()
		// SetKeepAlive 会触发库的 lazyInit（把内部 transport 赋给 Client.Transport），
		// 之后包装 Client.Transport 即对该客户端的所有出口请求生效（fetch.go:121 走 Client.Do）。
		hc.SetKeepAlive(keepAlive)
		inner := hc.Client.Transport
		if inner == nil {
			inner = http.DefaultTransport
		}
		if tr, ok := inner.(*http.Transport); ok && keepAlive {
			tr.MaxIdleConnsPerHost = 1 // 尽量固定同一后端连接
		}
		tr := &ipTracer{inner: inner}
		hc.Client.Transport = tr
		return pcs, tr
	}
	pcsA, trA = mk(true)
	pcsB, trB = mk(false)
	return pcsA, pcsB, trA, trB
}

// probeForcedIPClient 造一个「只连指定 IP」的库客户端（强制节点亲和），
// 用来判定「可见性差异是否由**命中的服务端节点**决定」。
func probeForcedIPClient(t *testing.T, bduss, ip string) (*bdlib.BaiduPCS, *ipTracer) {
	t.Helper()
	cli, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	pcs := cli.PCS()
	hc := pcs.GetClient()
	hc.SetKeepAlive(true)
	base, ok := hc.Client.Transport.(*http.Transport)
	if !ok || base == nil {
		t.Fatalf("底层 transport 不是 *http.Transport")
	}
	tr := base.Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, serr := net.SplitHostPort(addr)
		if serr == nil && (host == "pan.baidu.com" || strings.HasSuffix(host, ".baidu.com")) {
			addr = net.JoinHostPort(ip, port)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	tracer := &ipTracer{inner: tr}
	hc.Client.Transport = tracer
	return pcs, tracer
}

// probePhase 在一个阶段内轮询，直到 **CLI `ls -l` 判定收敛**（或超时），返回首次可见/收敛耗时。
func probePhase(t *testing.T, bin string, pcsA, pcsB, pcsD *bdlib.BaiduPCS, trA, trB, trD *ipTracer, dir, name string, maxWait time.Duration) (visIn, convIn time.Duration) {
	t.Helper()
	visIn, convIn = -1, -1
	t0 := time.Now()
	for {
		el := time.Since(t0)
		if el > maxWait {
			return visIn, convIn
		}
		trA.drain()
		visA, md5A, _ := probeListVia(pcsA, dir, name, nil)
		ipA := hostIPsOf(trA.drain())
		visB, _, _ := probeListVia(pcsB, dir, name, nil)
		visC, _, _ := probeListVia(pcsA, dir, name, bdlib.DefaultOrderOptions)
		visD, md5D, _ := probeListVia(pcsD, dir, name, nil)
		ipD := ipsOf(trD.drain())
		_ = ipsOf(trB.drain())
		cliVis, cliConv := probeCLIVerdict(bin, dir, name)
		t.Logf("  [T+%-5s] A(.107复用)可见=%-5v md5=%s ip=%v | B(新连接)可见=%-5v | C(DefaultOrder)可见=%-5v | D(强制.108)可见=%-5v md5=%s ip=%v | CLI-ls可见=%-5v 收敛=%-5v",
			el.Round(time.Second), visA, probeShort(md5A), ipA, visB, visC, visD, probeShort(md5D), ipD, cliVis, cliConv)
		if cliVis {
			if visIn < 0 {
				visIn = el
			}
			if cliConv {
				convIn = el
				return visIn, convIn
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// TestE2E_MD5ConvergenceProbe 见文件头：受控对照「连接复用 vs 每次新连接」。
func TestE2E_MD5ConvergenceProbe(t *testing.T) {
	// sproxy:serial: 真实百度链路 + 独占测试目录，天然串行
	root := probeRootBase(t)
	bduss := strings.TrimSpace(os.Getenv("BDUSS"))
	if bduss == "" {
		bduss = e2eBDUSSFromConfig(t)
	}
	if bduss == "" {
		t.Skip("BDUSS 为空（环境变量与 BaiduPCS-Go 配置都没读到）")
	}
	bin, ok := e2eBinaryAvailable()
	if !ok {
		t.Skip("BaiduPCS-Go 二进制不可用（PATH 或 ~/go/bin）")
	}

	size := probeEnvInt("MD5PROBE_SIZE", 8<<20)
	maxWait := probeEnvDur("MD5PROBE_WAIT", 60*time.Second)

	cli0, err := NewClient(bduss, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	dir := path.Join(root, fmt.Sprintf("md5conv-%d", time.Now().Unix()))
	if perr := cli0.PCS().Mkdir(dir); perr != nil {
		t.Fatalf("Mkdir %s: %v", dir, perr)
	}
	t.Cleanup(func() {
		if perr := cli0.PCS().Remove(dir); perr != nil {
			t.Logf("清理 %s 失败（可手工删除）: %v", dir, perr)
		}
	})

	pcsA, pcsB, trA, trB := probeSetupClients(t, bduss)
	// D：强制只连 112.34.111.108（与 tracer 常命中的 .107 互为不同节点）
	pcsD, trD := probeForcedIPClient(t, bduss, "112.34.111.108")
	t.Logf("== 参数 == size=%dB dir=%s maxWait=%s", size, dir, maxWait)

	local := filepath.Join(t.TempDir(), "probe.bin")
	if werr := probeMkRandFile(local, size); werr != nil {
		t.Fatalf("生成随机文件: %v", werr)
	}
	doUpload := func(label string) {
		t0 := time.Now()
		uctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		out, uerr := exec.CommandContext(uctx, bin, "upload", "--policy", "overwrite", local, dir+"/").CombinedOutput()
		if uerr != nil {
			t.Fatalf("%s 上传失败: %v\n%s", label, uerr, probeTruncate(string(out), 400))
		}
		t.Logf("-- %s 上传完成（CLI），耗时 %s", label, time.Since(t0).Round(time.Millisecond))
	}

	t.Logf("===== 阶段 1：首次上传 =====")
	doUpload("首传")
	vis1, conv1 := probePhase(t, bin, pcsA, pcsB, pcsD, trA, trB, trD, dir, "probe.bin", maxWait)
	t.Logf("阶段1 结果：首次可见=%s 首次收敛=%s", fmtDur(vis1), fmtDur(conv1))

	t.Logf("===== 阶段 2：再次上传（秒传）=====")
	doUpload("二次")
	vis2, conv2 := probePhase(t, bin, pcsA, pcsB, pcsD, trA, trB, trD, dir, "probe.bin", maxWait)
	t.Logf("阶段2 结果：可见=%s 收敛=%s", fmtDur(vis2), fmtDur(conv2))

	t.Logf("===== 汇总 ====")
	t.Logf("  首传后：CLI 首次可见 %s ／首次收敛 %s", fmtDur(vis1), fmtDur(conv1))
	t.Logf("  再传后：CLI 首次可见 %s ／首次收敛 %s", fmtDur(vis2), fmtDur(conv2))
}

// fmtDur 把观测耗时格式化为可读串（<0 表示未达成）。
func fmtDur(d time.Duration) string {
	if d < 0 {
		return "未达成（超时）"
	}
	return d.Round(time.Second).String()
}

// probeShort 截短 md5 便于日志对齐（空值显示 "-"）。
func probeShort(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// probeTruncate 截断长输出（仅用于错误信息展示）。
func probeTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
