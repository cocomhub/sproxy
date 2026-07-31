# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import sys

with open('pkg/client/client.go', 'r', encoding='utf-8') as f:
    content = f.read()

changes = 0

# 1. Add allowTransportFallback field to FileClient struct
old = '\tinitError       error         // WithTunnel/WithXfer 初始化错误\n}'
new = '\tinitError       error         // WithTunnel/WithXfer 初始化错误\n\tallowTransportFallback bool          // WithTransportFallback 设置后允许回退到直连模式\n}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('1. Field added')

# 2. Add WithTransportFallback function before calculateChecksum
old = '// calculateChecksum 计算文件的 SHA-256 十六进制摘要（无缓存版本）。'
new = '// WithTransportFallback 设置当隧道/xfer 初始化失败时允许回退到直连模式。\n// 默认情况下（不设置此选项），initError 会导致 doRequest 直接返回错误。\nfunc WithTransportFallback() Option {\n\treturn func(c *FileClient) {\n\t\tc.allowTransportFallback = true\n\t}\n}\n\n' + old
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('2. WithTransportFallback added')

# 3. Add ctx cancellation check in Upload goroutine
old = '\tgo func() {\n\t\tdefer pw.Close()\n\t\tdefer mw.Close()\n\t\tpart, wErr := mw.CreateFormFile("file", remoteClean)'
new = '\tgo func() {\n\t\tdefer pw.Close()\n\t\tdefer mw.Close()\n\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn\n\t\tdefault:\n\t\t}\n\t\tpart, wErr := mw.CreateFormFile("file", remoteClean)'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('3. Upload goroutine ctx cancel added')

# 4. Fix validateOutputPath to use containsPathTraversal
old = 'func validateOutputPath(path string) error {\n\tcleaned := filepath.Clean(path)\n\tif cleaned == "." {\n\t\treturn fmt.Errorf("输出路径不能为空")\n\t}\n\tif strings.Contains(cleaned, "..") {\n\t\treturn fmt.Errorf("输出路径不能包含路径穿越符 \'..\'")\n\t}\n\treturn nil\n}'
new = 'func validateOutputPath(path string) error {\n\tcleaned := filepath.Clean(path)\n\tif cleaned == "." {\n\t\treturn fmt.Errorf("输出路径不能为空")\n\t}\n\tif containsPathTraversal(cleaned) {\n\t\treturn fmt.Errorf("输出路径不能包含路径穿越符 \'..\'")\n\t}\n\treturn nil\n}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('4. validateOutputPath fixed')

# 5. Fix Download inline check
old = 'if outputPath == "" {\n\t\toutputPath = filename\n\t\tif strings.Contains(filepath.Clean(outputPath), "..") {\n\t\t\treturn fmt.Errorf("文件名不能包含路径穿越符 \'..\'")'
new = 'if outputPath == "" {\n\t\toutputPath = filename\n\t\tif containsPathTraversal(filepath.Clean(outputPath)) {\n\t\t\treturn fmt.Errorf("文件名不能包含路径穿越符 \'..\'")'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('5. Download inline check fixed')

# 6. Add ensureParentDir before os.Create in Download
old = '\tout, err := os.Create(outputPath)\n\tif err != nil {\n\t\treturn fmt.Errorf("创建文件失败: %w", err)\n\t}'
new = '\t// 创建父目录（如果不存在）\n\tif err := ensureParentDir(outputPath); err != nil {\n\t\treturn fmt.Errorf("创建输出目录失败: %w", err)\n\t}\n\tout, err := os.Create(outputPath)\n\tif err != nil {\n\t\treturn fmt.Errorf("创建文件失败: %w", err)\n\t}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('6. ensureParentDir added')

# 7. Add buildSubdirPath method before List
old = '// List 列出 sproxy 服务端上的文件，返回 name + size + checksum 的结构化列表。'
new = '// buildSubdirPath 将子目录参数拼接为路径，并检查路径穿越。\n// 返回 URL 编码后的路径字符串，可用于 URL query 参数。\nfunc (c *FileClient) buildSubdirPath(subdirs []string) (string, error) {\n\tsubdir := path.Join(append([]string{"/"}, subdirs...)...)\n\tif containsPathTraversal(subdir) {\n\t\treturn "", fmt.Errorf("路径不能包含 \'..\'")\n\t}\n\treturn url.QueryEscape(subdir), nil\n}\n\n' + old
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('7. buildSubdirPath added')

# 8. Fix List to use buildSubdirPath
old = 'func (c *FileClient) List(ctx context.Context, subdirs ...string) ([]FileInfo, error) {\n\theaders := make(http.Header)\n\tsubdir := path.Join(append([]string{"/"}, subdirs...)...)\n\tif strings.Contains(subdir, "..") {\n\t\treturn nil, fmt.Errorf("路径不能包含 \'..\'")\n\t}\n\tresp, err := c.doRequest(ctx, "GET", "/api/files?subdir="+url.QueryEscape(subdir), nil, headers)'
new = 'func (c *FileClient) List(ctx context.Context, subdirs ...string) ([]FileInfo, error) {\n\theaders := make(http.Header)\n\tsubdir, err := c.buildSubdirPath(subdirs)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tresp, err := c.doRequest(ctx, "GET", "/api/files?subdir="+subdir, nil, headers)'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('8. List buildSubdirPath')

# 9. Fix ListWithPagination to use buildSubdirPath
old = 'func (c *FileClient) ListWithPagination(ctx context.Context, offset, limit int, subdirs ...string) ([]FileInfo, int, error) {\n\theaders := make(http.Header)\n\tsubdir := path.Join(append([]string{"/"}, subdirs...)...)\n\tif strings.Contains(subdir, "..") {\n\t\treturn nil, 0, fmt.Errorf("路径不能包含 \'..\'")\n\t}\n\turlPath := fmt.Sprintf("/api/files?subdir=%s&offset=%d&limit=%d", url.QueryEscape(subdir), offset, limit)'
new = 'func (c *FileClient) ListWithPagination(ctx context.Context, offset, limit int, subdirs ...string) ([]FileInfo, int, error) {\n\theaders := make(http.Header)\n\tsubdir, err := c.buildSubdirPath(subdirs)\n\tif err != nil {\n\t\treturn nil, 0, err\n\t}\n\turlPath := fmt.Sprintf("/api/files?subdir=%s&offset=%d&limit=%d", subdir, offset, limit)'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('9. ListWithPagination buildSubdirPath')

# 10. Fix initError fallback logic in doRequest
old = '\tif c.initError != nil {\n\t\tc.logger.Warn("隧道不可用，回退到直连模式", "init_error", c.initError)\n\t}'
new = '\tif c.initError != nil {\n\t\tif !c.allowTransportFallback {\n\t\t\treturn nil, fmt.Errorf("transport initialization failed: %w", c.initError)\n\t\t}\n\t\tc.logger.Warn("transport unavailable, falling back to direct mode", "init_error", c.initError)\n\t}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('10. initError fallback fixed')

# 11. Fix successChecker interface
old = 'type successChecker interface {\n\tisSuccess() bool\n}'
new = 'type successChecker interface {\n\tisSuccess() bool\n\tmessage() string\n}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('11. successChecker interface fixed')

# 12. Fix doJSONResp GetMessage() -> message()
old = 'func (r *doJSONResp) GetMessage() string { return r.Message }'
new = 'func (r *doJSONResp) message() string { return r.Message }'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('12. doJSONResp message()')

# 13. Fix UploadResult GetMessage() -> message()
old = 'func (r *UploadResult) GetMessage() string { return r.Message }'
new = 'func (r *UploadResult) message() string { return r.Message }'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('13. UploadResult message()')

# 14. Fix doJSON auto-check
old = '\t\t// 自动检查 Success 字段\n\t\tif checker, ok := respBody.(successChecker); ok && !checker.isSuccess() {\n\t\t\tmsg := ""\n\t\t\tif m, ok := respBody.(interface{ GetMessage() string }); ok {\n\t\t\t\tmsg = m.GetMessage()\n\t\t\t}\n\t\t\treturn fmt.Errorf("请求失败: %s", msg)\n\t\t}'
new = '\t\t// 自动检查 Success 字段\n\t\tif checker, ok := respBody.(successChecker); ok && !checker.isSuccess() {\n\t\t\treturn fmt.Errorf("请求失败: %s", checker.message())\n\t\t}'
if old in content:
    content = content.replace(old, new, 1)
    changes += 1
    print('14. doJSON auto-check fixed')

with open('pkg/client/client.go', 'w', encoding='utf-8') as f:
    f.write(content)

print(f'\nTotal changes: {changes}')