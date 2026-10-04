// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// secrets.go 是 secret 卷的客户端（FileClient）封装：CLI `sclient secret` 命令族
// 的服务端调用面（POST/GET/DELETE /api/secrets）。服务端只校验+落盘，不参与派生；
// 双口令派生在本地（DerivePassphraseSecret），随机生成由服务端完成并返回。
//
// 传输安全：本封装经 FileClient.doRequest（SproxySig 签名/隧道自动处理）——secret
// 值（hex）是敏感数据，走既有加密通道；原始口令永不上线（只传派生结果）。

// SecretCreateOptions 是创建 secret 的参数。
type SecretCreateOptions struct {
	Name   string
	Mode   string // "random"（缺省）或 "import"/"passphrase"
	Value  string // import/passphrase 模式的派生结果（64 小写 hex）
	Origin string // 来源标注（random/passphrase/import）
}

// SecretCreateResult 是 POST /api/secrets 响应。
type SecretCreateResult struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"` // 仅 random 模式返回（供立即备份）
	Origin  string `json:"origin"`
	Message string `json:"message,omitempty"`
}

// CreateSecret 创建（追加/覆盖）一个 secret：
//   - mode=random：服务端生成随机 32B→hex，返回 secret 值（供立即备份）；
//   - mode=import/passphrase：上传本地派生结果（64 小写 hex）。
func (c *FileClient) CreateSecret(ctx context.Context, opts SecretCreateOptions) (*SecretCreateResult, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, fmt.Errorf("secret 名不能为空")
	}
	req := struct {
		Name   string `json:"name"`
		Mode   string `json:"mode"`
		Value  string `json:"value"`
		Origin string `json:"origin"`
	}{
		Name: name, Mode: opts.Mode, Value: opts.Value, Origin: opts.Origin,
	}
	var resp SecretCreateResult
	if err := c.doJSON(ctx, "POST", "/api/secrets", &req, &resp); err != nil {
		return nil, fmt.Errorf("创建 secret %q 失败: %w", name, err)
	}
	return &resp, nil
}

// CreateRandomSecret 便捷封装：请求服务端生成随机 secret。
func (c *FileClient) CreateRandomSecret(ctx context.Context, name string) (*SecretCreateResult, error) {
	return c.CreateSecret(ctx, SecretCreateOptions{Name: name, Mode: "random"})
}

// CreatePassphraseSecret 便捷封装：本地双口令派生后上传派生结果。
// 口令在调用方处由交互输入取得（x/term.ReadPassword），**不进入本方法之外的任何
// 存储/传输**；本方法只把派生 hex 上传。
func (c *FileClient) CreatePassphraseSecret(ctx context.Context, name, passA, passB string) (*SecretCreateResult, error) {
	derived, err := secrets.DerivePassphraseSecret(passA, passB)
	if err != nil {
		return nil, err
	}
	return c.CreateSecret(ctx, SecretCreateOptions{
		Name: name, Mode: "passphrase", Value: string(derived), Origin: "passphrase",
	})
}

// ListSecrets 列出 secret 卷内全部 secret 名（GET /api/secrets）。
func (c *FileClient) ListSecrets(ctx context.Context) ([]string, error) {
	var out struct {
		Secrets []string `json:"secrets"`
	}
	if err := c.doJSON(ctx, "GET", "/api/secrets", nil, &out); err != nil {
		return nil, fmt.Errorf("获取 secret 列表失败: %w", err)
	}
	return out.Secrets, nil
}

// ExportSecret 导出 secret 内容（明文 hex；GET /api/secrets/{name}）。
// 返回的 secret 值供备份/迁移；调用方负责后续保护（CLI 默认口令加密落盘）。
func (c *FileClient) ExportSecret(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("secret 名不能为空")
	}
	path := "/api/secrets/" + url.PathEscape(name)
	var resp SecretCreateResult
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return "", fmt.Errorf("导出 secret %q 失败: %w", name, err)
	}
	return resp.Value, nil
}

// DeleteSecret 删除 secret（DELETE /api/secrets/{name}）。
func (c *FileClient) DeleteSecret(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("secret 名不能为空")
	}
	path := "/api/secrets/" + url.PathEscape(name)
	resp, err := c.doRequest(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return fmt.Errorf("删除 secret %q 失败: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("删除 secret %q 失败 (HTTP %d): %s", name, resp.StatusCode, string(body))
	}
	return nil
}
