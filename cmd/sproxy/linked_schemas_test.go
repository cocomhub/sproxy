// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// linked_schemas_test.go 验证 linked（外部）后端基础建卷 schema（D2，2026-10-06）：
//   - webdav/sftp/ftp/s3/baidupcs 登记静态建卷 schema，/api/backends 返回非空 fields；
//   - 字段与各后端 NewBackend 实际消费的 Extra 键一致（url/凭据必填，可填后由后端
//     fail-fast 校验，不再「类型字段全空导致缺必填键」恒 400）。
//
// 串行登记（registerLinkedBackendSchemasOnce）与生产装配（setupSyncVolumeBackends）共享，
// 测试调用仅触发首次登记；读取经 registry.BackendSchema（静态表优先，构造无关）。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

func TestRegisterLinkedBackendSchemas(t *testing.T) {
	// sproxy:serial: 生产装配 Once 全局单例（共享静态 schema 表）。
	registerLinkedBackendSchemas()

	cases := []struct {
		typ        string
		required   []string // 断言 Required=true 的键（各后端单键必填）
		present    []string // 断言存在的键（含可选键）
		wantFields int
	}{
		{typ: "webdav", required: []string{"url"}, present: []string{"url", "username", "password", "token"}, wantFields: 4},
		{typ: "sftp", required: []string{"url"}, present: []string{"url", "password", "private_key", "root"}, wantFields: 4},
		{typ: "ftp", required: []string{"url", "password"}, present: []string{"url", "password", "root"}, wantFields: 3},
		// s3：endpoint/bucket/access_key/secret_key 单键必填；region/use_ssl 可选。
		{typ: "s3", required: []string{"endpoint", "bucket", "access_key", "secret_key"}, present: []string{"endpoint", "bucket", "access_key", "secret_key", "region", "use_ssl"}, wantFields: 6},
		// baidupcs：bduss 或 binary_path 至少一个（组必填，非单键必填）——断言两者都在表单可填。
		{typ: "baidupcs", required: nil, present: []string{"bduss", "binary_path", "baidu_root", "local_root"}, wantFields: 4},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			fields := registry.BackendSchema(c.typ)
			if len(fields) == 0 {
				t.Fatalf("%q 应登记静态建卷 schema（fields 非空），got nil", c.typ)
			}
			if len(fields) != c.wantFields {
				t.Fatalf("%q fields 长度 = %d, want %d (%+v)", c.typ, len(fields), c.wantFields, fields)
			}
			for _, k := range c.present {
				f := fieldByKey(fields, k)
				if f == nil {
					t.Fatalf("%q schema 缺键 %q（已登记字段 %+v）", c.typ, k, fields)
				}
				if f.Type != "text" && f.Type != "bool" {
					t.Errorf("%q schema 字段 %q 类型应 text/bool，got %q", c.typ, k, f.Type)
				}
			}
			for _, k := range c.required {
				if f := fieldByKey(fields, k); f == nil || !f.Required {
					t.Errorf("%q schema 单键必填字段 %q 缺失或未标 Required", c.typ, k)
				}
			}
		})
	}
}

// fieldByKey 在 schema 字段列表中按 key 查找（nil = 不存在）。
func fieldByKey(fields []registry.FieldSchema, key string) *registry.FieldSchema {
	for i := range fields {
		if fields[i].Key == key {
			return &fields[i]
		}
	}
	return nil
}
