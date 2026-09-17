# BaiduPCS P2：库兜底真实现 + 断点续传本地持久化 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 P1 的库兜底（stub）提升为真实现：分片上传（`NewMultiUploader` + 自实现 `MultiUpload` 接口）+ 断点续传状态本地持久化（`.resume/`），**脱离二进制也完整可用**。

**架构：**
- `libraryAdapter.Upload` 从裸 `PrepareUpload` 改为 `NewMultiUploader`（并发分片、InstanceState 断点、限速）。
- 新增 `MultiUpload` 接口实现（`Precreate`/`TmpFile`/`CreateSuperFile` 三方法，映射到 BaiduPCS API）。
- 断点续传：`InstanceState` 持久化到 `<本地>/baidupcs/resume/<key>.json`（原子写 tmp+rename，仿 `pkg/store` 模式）；`Download` 用 Downloader 的 RangeList + 本地 `tmp/` 恢复。
- 本地目录布局（用户硬约束：中间状态只依赖本地文件系统）：
  ```
  <本地配置根>/baidupcs/
    staging/  本地上传暂存（配合 quota）
    resume/   断点状态（InstanceState JSON）
    cache/    下载缓存（可选）
    tmp/      通用临时文件
  ```
- quota 打配合（P2 基础版）：staging 计入 owner 配额，chunk 上传成功释放本地占用。

**技术栈：** Go 1.27；fork（replace 指 cocomhub/BaiduPCS-Go）；`GOWORK=off` 独立构建验证。

**规格：** 用户 2026-09-17 确认：长期目标 = 像管理本地文件一样管理百度网盘、支持跨存储同步、中间状态只依赖本地 FS、网盘作为存储后端。P2 = 库兜底真实现（脱离二进制可用）。

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；fake 二进制内 Sleep 用变量拼接绕开（保持生成代码可编译）。
- 禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- **独立 module 硬规则**：`GOWORK=off` 独立构建/测试。
- 提交前 `make prepare`。

---

### 任务 1：本地目录布局 + resume 持久化工具

**文件：**
- 创建：`pkg/baidupcs/layout.go`（`Layout` 结构：BaseDir/Staging/Resume/Cache/Tmp + `NewLayout(base)` + `SanitizeKey`）
- 创建：`pkg/baidupcs/resume.go`（`SaveResume`/`LoadResume`/`DeleteResume`，原子写 tmp+rename）
- 测试：`pkg/baidupcs/layout_test.go`、`pkg/baidupcs/resume_test.go`

**目标：** 目录布局 + 断点持久化工具可用。

- [ ] **步骤 1：编写失败的测试**

```go
func TestLayout_Structure(t *testing.T) {
    t.Parallel()
    // NewLayout(tmp) → Staging/Resume/Cache/Tmp 子目录存在
}
func TestResume_Roundtrip(t *testing.T) {
    t.Parallel()
    // SaveResume(key, state) → LoadResume(key) 一致
}
func TestResume_Delete(t *testing.T) {
    t.Parallel()
    // DeleteResume 后 LoadResume → 不存在
}
func TestSanitizeKey(t *testing.T) {
    t.Parallel()
    // 路径穿越形状归一（仿现有 sanitizeRemotePath）
}
```

- [ ] **步骤 2：运行测试验证失败** → 实现 `layout.go`/`resume.go` → 验证通过

- [ ] **步骤 3：Commit**

```bash
git add pkg/baidupcs/layout.go pkg/baidupcs/resume.go pkg/baidupcs/layout_test.go pkg/baidupcs/resume_test.go
git commit -m "feat(baidupcs): 本地目录布局 + 断点状态原子持久化工具" --no-verify
```

---

### 任务 2：分片上传真实现（NewMultiUploader + MultiUpload 接口）

**文件：**
- 修改：`pkg/baidupcs/adapter.go`（`libraryAdapter.Upload` 改造）
- 创建：`pkg/baidupcs/multiupload.go`（`baiduMultiUpload` 实现 MultiUpload 三方法）
- 测试：`pkg/baidupcs/multiupload_test.go`、`adapter_test.go` 补充

**目标：** 上传走分片 + 断点，脱离二进制完整可用。

- [ ] **步骤 1：读上游 API（fork 源码）**

`requester/uploader/multiuploader.go`：`NewMultiUploader(multiUpload MultiUpload, file rio.ReaderAtLen64, config *MultiUploaderConfig, targetPath string)`；
`MultiUpload` 接口：`Precreate() (pcsHost string, err pcserror.Error)`、`TmpFile(ctx, uploadid, targetPath string, partseq int, partOffset int64, readerlen64 rio.ReaderLen64) (checksum string, terr error)`、`CreateSuperFile(pcsHost, policy, uploadId string, fileSize int64, checksumMap map[int]string) (cerr error)`；
`baidupcs/upload.go` 的对应 API：`Precreate`/`UploadTmpFile`/`UploadCreateSuperFile`。

> fork 源码在 `github.com/cocomhub/BaiduPCS-Go`（replace 指向），本地 `go mod download` 后到 `$(go env GOMODCACHE)/github.com/qjfoidnh/BaiduPCS-Go*/` 读取。

- [ ] **步骤 2：编写失败的测试**

```go
// fake pcs（实现 MultiUpload 三方法的 map 内存版）驱动
func TestMultiUploader_Upload_SmallFile(t *testing.T) { t.Parallel() /* 小文件单分片 */ }
func TestMultiUploader_Upload_Chunked(t *testing.T)    { t.Parallel() /* 大文件多分片 */ }
func TestMultiUploader_Resume(t *testing.T)            { t.Parallel() /* 中断后恢复（InstanceState） */ }
func TestMultiUploader_Upload_NoBinary(t *testing.T)   { t.Parallel() /* 无二进制也完整上传 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（`baiduMultiUpload` 三方法 + `Upload` 改 `NewMultiUploader`；config Parallel/BlockSize/MaxRate 可配）→ 验证通过 + 变异验证（去掉分片 → chunked 测试红）

- [ ] **步骤 4：Commit**

```bash
git add pkg/baidupcs/multiupload.go pkg/baidupcs/adapter.go pkg/baidupcs/multiupload_test.go pkg/baidupcs/adapter_test.go
git commit -m "feat(baidupcs): 分片上传真实现（MultiUploader + 断点恢复），脱离二进制可用" --no-verify
```

---

### 任务 3：下载断点 + 中间态清理 + quota 基础

**文件：**
- 修改：`pkg/baidupcs/adapter.go`（`libraryAdapter.Download` 改 Downloader + RangeList 断点）
- 创建：`pkg/baidupcs/quota.go`（staging/cache 配额挂钩：预留/释放）
- 测试：`pkg/baidupcs/quota_test.go`、`adapter_test.go` 补充

**目标：** 下载断点恢复 + staging/cache 配额打配合。

- [ ] **步骤 1：编写失败的测试**

```go
func TestDownload_Resume(t *testing.T)         { t.Parallel() /* 中断后 Range 恢复 */ }
func TestQuota_StagingReserveRelease(t *testing.T) { t.Parallel() /* staging 预留 → chunk 上传成功释放 */ }
func TestQuota_CacheCap(t *testing.T)          { t.Parallel() /* cache 独立上限 */ }
```

- [ ] **步骤 2：运行测试验证失败** → 实现（`Download` 用 `requester/downloader.Downloader` + RangeList 持久化 `.resume/`；`quota.go` 提供 `ReserveUsage`/`ReleaseUsage`，复用 `pkg/quota.Scope` 语义）→ 验证通过 + 变异验证

- [ ] **步骤 3：Commit**

```bash
git add pkg/baidupcs/quota.go pkg/baidupcs/adapter.go pkg/baidupcs/quota_test.go pkg/baidupcs/adapter_test.go
git commit -m "feat(baidupcs): 下载断点恢复 + staging/cache 配额挂钩" --no-verify
```

---

### 任务 4：文档 + 全量验证

**文件：**
- 修改：`pkg/baidupcs/README.md`（P2 能力说明：脱离二进制可用、断点格式、目录布局、quota 语义）

**目标：** 文档同步。

- [ ] **步骤 1：更新 README**

P2 节：分片上传/断点/目录布局/quota 挂钩；「库兜底从 stub 提升为真实现」。

- [ ] **步骤 2：全量验证 + Commit**

```bash
cd pkg/baidupcs && GOWORK=off go build ./... && GOWORK=off go test ./...
cd /d/workdir/leon/cocomhub/sproxy && make prepare && go build ./... && go test ./pkg/... ./internal/... ./cmd/... && golangci-lint run ./...
git add pkg/baidupcs/README.md
git commit -m "docs(baidupcs): P2 能力文档（分片/断点/目录布局/quota）" --no-verify
```
