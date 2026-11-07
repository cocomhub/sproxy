// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// linked_schemas.go 登记 linked（外部）后端的基础建卷 schema（D2 修复，2026-10-06）。
//
// 背景：webdav/sftp/ftp/s3/baidupcs 这些 linked 后端未实现 registry.SchemaProvider
// （pkg/volume/registry：可选接口，实现者用 Schema() 声明建卷表单），BackendSchema 构造
// 读取为空 → GET /api/backends 对这些类型返回 fields: [] → Web UI「卷管理」建卷表单只剩
// name/capacity，缺后端必填的 url/凭据字段 → 建卷恒 400（后端 fail-fast 报缺 extra）。
//
// 修法（最小方案，不逐个深挖后端内部构造）：
//   - 在装配层登记**静态**创建表单 schema（registry.RegisterBackendSchema，构造无关，
//     与 registerSecretSchemas 同模式），字段与各后端 newXxxBackend 实际消费的 Extra 键一致；
//   - UI 表单渲染这些 text/number/bool 字段 → 用户可填 → 提交后由后端自带 fail-fast
//     校验 extra（错误文案回显），不再「类型字段全空导致缺必填键」。
//
// 各后端必填/可选键（以各后端 newXxxBackend 读 v.Extra 为准，值须为 string/bool/number）：
//   - webdav：url 必填；username+password 或 token 至少一组（Basic/Bearer 认证）；
//   - sftp：url 必填；password 或 private_key 至少一个；root 可选；
//   - ftp：url 必填（含 ftp://user@host）；password 必填；root 可选；
//   - s3：endpoint/bucket/access_key/secret_key 必填；region/use_ssl(布尔) 可选；
//   - baidupcs：bduss 或 binary_path 至少一个；baidu_root/local_root 可选。
//
// 装配时机：同步后端注册点（setupSyncVolumeBackends）——linked 后端仅在 sync 装配时
// register 进 registry，schema 与类型同步登记，/api/backends 恒先看到类型后有正常表单。
// registerLinkedBackendSchemasOnce 守卫（RegisterBackendSchema 重复登记即 panic）。
var registerLinkedBackendSchemasOnce sync.Once

// registerLinkedBackendSchemas 登记 linked（外部）类型的基础建卷 schema。
func registerLinkedBackendSchemas() {
	registerLinkedBackendSchemasOnce.Do(func() {
		registerLinkedBackendSchema("webdav", []registry.FieldSchema{
			{Key: "url", Label: "WebDAV 地址", Type: "text", Required: true},
			{Key: "username", Label: "用户名", Type: "text"},
			{Key: "password", Label: "密码", Type: "text"},
			{Key: "token", Label: "Token", Type: "text"},
		})
		registerLinkedBackendSchema("sftp", []registry.FieldSchema{
			{Key: "url", Label: "SFTP 地址", Type: "text", Required: true},
			{Key: "password", Label: "密码", Type: "text"},
			{Key: "private_key", Label: "SSH 私钥内容", Type: "text"},
			{Key: "root", Label: "远端根目录", Type: "text"},
		})
		registerLinkedBackendSchema("ftp", []registry.FieldSchema{
			{Key: "url", Label: "FTP 地址", Type: "text", Required: true},
			{Key: "password", Label: "密码", Type: "text", Required: true},
			{Key: "root", Label: "远端根目录", Type: "text"},
		})
		registerLinkedBackendSchema("s3", []registry.FieldSchema{
			{Key: "endpoint", Label: "S3 服务地址", Type: "text", Required: true},
			{Key: "bucket", Label: "桶名", Type: "text", Required: true},
			{Key: "access_key", Label: "Access Key", Type: "text", Required: true},
			{Key: "secret_key", Label: "Secret Key", Type: "text", Required: true},
			{Key: "region", Label: "区域", Type: "text"},
			{Key: "use_ssl", Label: "使用 TLS", Type: "bool"},
		})
		registerLinkedBackendSchema("baidupcs", []registry.FieldSchema{
			{Key: "bduss", Label: "BDUSS 凭据", Type: "text"},
			{Key: "binary_path", Label: "BaiduPCS-Go 路径", Type: "text"},
			{Key: "baidu_root", Label: "网盘根路径", Type: "text"},
			{Key: "local_root", Label: "本地中间态目录", Type: "text"},
		})
	})
}

// registerLinkedBackendSchema 为单个 linked 类型登记静态建卷 schema（空字段防御：不应发生）。
func registerLinkedBackendSchema(typ string, fields []registry.FieldSchema) {
	if len(fields) == 0 {
		return // 防御：不登记空 schema（RegisterBackendSchema 空字段 panic）
	}
	registry.RegisterBackendSchema(typ, fields)
}
