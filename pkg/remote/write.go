// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// write.go 是 A 侧 `pkg/remote` 的**写面**（Y 二期 P3-c）：把 `sync.FS` 的 4 个写方法翻成
// 对端写 listener 的 4 条 POST，**不实现任何写语义**（checksum 门禁、原子改名、版本、配额、
// 文件锁、卷路由全在对端 `pkg/files` 的域方法里）。
//
// 两条链路的分工（本片的结构事实）：
//   - 写操作走**写面**（独立服务名 `volwrite`、独立 listener、独立路由白名单）；
//   - `Rename`/`Delete` 的 checksum 前置条件走**读面**（`/remote/stat`）——对端写面只有写 op，
//     没有 stat，故 A 侧必须「先 Stat 取 checksum，再带进写请求」。
//
// 为什么 `WriteFile` 要先 spool：对端要求 `X-File-Checksum` 为**前置**声明（不符即拒并删除
// 已写内容），而流式上传时摘要要到读完整流才知道 ⇒ 先落临时文件并同时算 SHA-256，再单次
// 流式提交（内存占用有界，网络只走一遍）。
package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ServiceNameWrite 是写面的 mesh 服务名（写面与只读面**分服务名**：独立 listener、独立
// 路由白名单；只读通路物理上不含写 handler）。
const ServiceNameWrite = "volwrite"

// WithWriteDialer 设置**写面**拨号器（缺省 nil = 写操作 fail-closed 报错）。
//
// 读面拨号器由 `New(dialer)` 提供；写面通常是另一个服务名（`volwrite`）的拨号器，故单独注入。
func WithWriteDialer(d Dialer) Option {
	return func(c *Client) { c.writeDialer = d }
}

// ErrWriteNotConfigured 表示本端未配置写面拨号器（未开启写批次）。写操作 fail-closed：
// 既不静默成功（数据丢失型缺陷），也不复用读面链路（那会绕过「写面独立授权/独立路由」）。
var ErrWriteNotConfigured = errors.New("remote: 写面未配置（WithWriteDialer）")

// WriteFile 把 r 的内容写入 ref（对端写面 POST /remote/write）。
//
// size 是调用方声明的字节数（`sync.FS` 传 Stat 得到的 size）；实际 spool 字节数与它不符时
// 报错——这是**调用方 bug 的早期暴露**（对端只认内容与其 checksum）。
// mtime 为 Unix 秒（0 = 不设置时间戳），透传为 `X-File-MTime`（纳秒），语义与本地上传一致。
func (c *Client) WriteFile(ctx context.Context, ref Ref, r io.Reader, size, mtime int64) error {
	if err := c.requireWrite(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("remote: WriteFile 的源为空")
	}
	// spool：同时算 SHA-256（对端要前置 checksum）。
	tmp, err := os.CreateTemp("", "sproxy-remote-write-*")
	if err != nil {
		return fmt.Errorf("remote: 创建 spool 临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return fmt.Errorf("remote: 读取待写入内容失败: %w", err)
	}
	if size >= 0 && size != written {
		return fmt.Errorf("remote: WriteFile 声明字节数 %d 与实际读取 %d 不符", size, written)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("remote: spool 回绕失败: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	headers := map[string]string{headerFileChecksum: sum}
	if mtime > 0 {
		// 与本地 X-File-MTime 同约定：UnixNano 字符串。
		headers[headerFileMTime] = strconv.FormatInt(mtime*int64(1e9), 10)
	}
	return c.writeOK(ctx, ref, "/remote/write", nil, tmp, headers, "写入")
}

// Rename 把 from 改名/移动到 to（对端写面 POST /remote/rename）。
//
// **先 Stat 取 checksum**：对端要求源文件 checksum 前置（防误覆盖/误改名），而写面没有 stat，
// 故必须经读面拿。源不存在或对端未提供 checksum 时**不发起写请求**（fail-closed）。
func (c *Client) Rename(ctx context.Context, from, to Ref) error {
	// 先查写面配置（未配置就**不发任何请求**——包括读面的 checksum 探测）。
	if err := c.requireWrite(); err != nil {
		return err
	}
	if err := sameTarget(from, to); err != nil {
		return err
	}
	checksum, err := c.checksumFor(ctx, from)
	if err != nil {
		return err
	}
	return c.writeOK(ctx, from, "/remote/rename",
		map[string]string{"from": from.Path, "to": to.Path}, nil,
		map[string]string{headerFileChecksum: checksum}, "改名")
}

// Delete 删除 ref（对端写面 POST /remote/delete）；同样**先 Stat 取 checksum**。
func (c *Client) Delete(ctx context.Context, ref Ref) error {
	if err := c.requireWrite(); err != nil {
		return err
	}
	checksum, err := c.checksumFor(ctx, ref)
	if err != nil {
		return err
	}
	return c.writeOK(ctx, ref, "/remote/delete", nil, nil,
		map[string]string{headerFileChecksum: checksum}, "删除")
}

// MakeDir 在 ref 路径建目录（对端写面 POST /remote/mkdir）。
func (c *Client) MakeDir(ctx context.Context, ref Ref) error {
	if err := c.requireWrite(); err != nil {
		return err
	}
	return c.writeOK(ctx, ref, "/remote/mkdir", nil, nil, nil, "建目录")
}

// requireWrite 在写操作前检查写面已配置：未配置则**立即**返回 ErrWriteNotConfigured，
// **不产生任何网络请求**（含读面的 checksum 探测）。linkForWrite 内另有一道同义检查
// （纵深防御：任何新写入口都绕不过）。
func (c *Client) requireWrite() error {
	if c.writeDialer == nil {
		return ErrWriteNotConfigured
	}
	return nil
}

// checksumFor 经**读面** Stat 取 ref 的 checksum；不存在或 checksum 缺失即报错（fail-closed）。
func (c *Client) checksumFor(ctx context.Context, ref Ref) (string, error) {
	fi, err := c.Stat(ctx, ref)
	if err != nil {
		return "", err
	}
	if fi == nil {
		return "", fmt.Errorf("remote: %s 不存在（无法满足写面的 checksum 前置）", ref.Path)
	}
	if strings.TrimSpace(fi.Checksum) == "" {
		return "", fmt.Errorf("remote: 对端未提供 %s 的 checksum（无法满足写面的 checksum 前置）", ref.Path)
	}
	return fi.Checksum, nil
}

// sameTarget 校验改名两端在同一节点与卷（跨节点/跨卷改名不是本层语义）。
func sameTarget(from, to Ref) error {
	if from.Node != to.Node {
		return fmt.Errorf("remote: 不支持跨节点改名（%s → %s）", from.Node, to.Node)
	}
	if from.Volume != to.Volume {
		return fmt.Errorf("remote: 不支持跨卷改名（%s → %s）", from.Volume, to.Volume)
	}
	if to.Path == "" {
		return errors.New("remote: 改名目标路径为空")
	}
	return nil
}

// writeOK 在写面链路上发一次 POST 并要求 2xx；非 2xx 转成错误（带状态码与对端通用文案）。
func (c *Client) writeOK(ctx context.Context, ref Ref, path string, query map[string]string, body io.Reader, headers map[string]string, op string) error {
	resp, err := c.writeDo(ctx, ref, path, query, body, headers)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// 对端只回通用文案（不泄露卷/文件存在性）；这里也把体限长读入错误信息以便排障。
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("remote: %s失败: status=%d %s", op, resp.StatusCode, strings.TrimSpace(string(msg)))
}

// writeDo 在**写面**链路上执行一次写请求（查询参数与读面同构：volume 恒有，path 非空才带）。
func (c *Client) writeDo(ctx context.Context, ref Ref, path string, query map[string]string, body io.Reader, headers map[string]string) (*http.Response, error) {
	l, err := c.linkForWrite(ctx, ref.Node)
	if err != nil {
		return nil, err
	}
	q := make([]string, 0, len(query)+2)
	q = append(q, "volume="+url.QueryEscape(ref.Volume))
	if ref.Path != "" {
		q = append(q, "path="+url.QueryEscape(ref.Path))
	}
	for k, v := range query {
		// from/to 允许空串以外的值；空值不发送（对端按缺参拒绝）。
		if v == "" {
			continue
		}
		q = append(q, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	sort.Strings(q)
	target := path + "?" + strings.Join(q, "&")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, fmt.Errorf("remote: 构造写请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := l.tun.Do(req)
	if err != nil {
		// 链路可能已失效：移出缓存，下次调用重建（当前请求不重试——写语义由调用方决定）。
		c.dropWriteLink(ref.Node, l)
		return nil, fmt.Errorf("remote: 写面隧道请求 %s 失败: %w", path, err)
	}
	return resp, nil
}
