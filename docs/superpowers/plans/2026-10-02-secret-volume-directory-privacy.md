# secret 加密卷目录名保密（初版实现）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 PR #723 的 secret 加密卷初版实现（shardseal 分块加密 + secrets/secret_data 封装卷）按设计文档补齐「目录名保密」：底层全随机可见名称（目录随机 5-30、文件三段 hex 同构）、meta 内容加密 + padding、目录 meta（@）存逻辑路径、文件 meta 只存 basename。

**架构：** 底层布局为「逻辑目录 ↔ 随机命名容器目录（5-30 字符）」；容器内混放三类同构文件（分块无标记、文件 meta 含 `-`/`_`、目录 meta 含 `@`），靠文件名标记分类。meta 明文 = `[4B jsonLen][metaJSON][rand padding]` 整体加密，落盘统一 `[R 128B 随机首部][4B 密文长][salt][nonce][ct+tag]`——首部/大小/长度三维度均无 meta 特征。逻辑路径 = 目录 meta.path + 文件 meta.basename。

**技术栈：** Go 1.27（仅 stdlib + `golang.org/x/crypto` + `gopkg.in/yaml.v3`），`pkg/cryptox/shardseal`、`pkg/volume/secretdata`、`pkg/volume/secrets`、`cmd/sproxy`。

**规格：** `docs/designs/2026-10-01-secret-volume.md`（初版含目录名保密设计，本计划实现其 §3/§4.2/§5/§6.2/§10 全部要求）

## 全局约束

- Go 1.27；仅 stdlib + `golang.org/x/crypto`（scrypt）+ `gopkg.in/yaml.v3`；**禁止新增第三方依赖**（sproxy 严格 stdlib 政策，x/ 可自由用）。
- 所有 .go 文件携带 SPDX 头：`Copyright 2026 The Cocomhub Authors. All rights reserved.` / `SPDX-License-Identifier: Apache-2.0`；UTF-8 无 BOM。
- 测试纯标准库（禁 testify/gomock/gomega）；新增测试默认 `t.Parallel()`（R18 门禁）；无 `http.DefaultClient`；本地 FS 用 `t.TempDir()`。
- 错误处理：哨兵错误 `var ErrXxx = errors.New(...)` + `fmt.Errorf("...: %w", err)` 包装。
- 日志统一 `log/slog`，不混入 zap/logrus。
- 提交信息遵循 Conventional Commits（feat/fix 等；subject 用户可读的能力描述；body 背景→改动要点→验证证据）。提交前必须 `go fmt` + `go vet` + `golangci-lint` 0 issues（门禁）。
- 命令均在 `D:\workdir\leon\cocomhub\sproxy-secret-worktree`（PR 分支 worktree）执行。

## 审查重点（Review Focus）

1. **文件名长度特征**：三类文件名长度必须同分布 54–62，任何一类不得明显长/短（如 `emeta-<hex>` 短名属违约）。→ 测试 `TestNameLengthUniform`（任务 1）。
2. **文件大小特征**：meta 落盘必须 padding 到分块范围，不得留 KB 级孤点；且长度头与文件大小线性一致（meta 与分块同式 `文件大小 = R + 4 + 32 + 12 + len(密文)`）。→ 测试 `TestMetaPadLengthInRange` + `TestUniformOnDiskFormat`（任务 3）。
3. **首部格式指纹**：文件首部必须是固定长度随机字节（R 段），`file`/magic 检测不可识别；篡改 R 段不得影响解密（R 仅混淆）。→ 测试 `TestUniformOnDiskFormat` + `TestMetaPadDecryptsExact`（任务 3）。
4. **逻辑路径恢复**：重启后从目录 meta.path + 文件 meta.basename 重建完整路径索引；子目录结构不丢失、不同目录同名文件不冲突（修复 F-1）。→ 测试 `TestLoadIndex_RestoresDirTree`（任务 5）。
5. **目录移动可解析**：目录移动仅更新目录 meta 的 path，文件 meta/分块零改动，重启后仍可解析。→ 测试 `TestDirMove_UpdatesMetaOnly`（任务 5）。

---

### 任务 1：shardseal 命名扩展（MetaName 三参 + 目录 meta/目录名 + 长度锁定）

**文件：**
- 修改：`pkg/cryptox/shardseal/naming.go`
- 修改：`pkg/cryptox/shardseal/shardseal.go`（`EncryptShards` 内 `MetaName` 调用点）
- 测试：`pkg/cryptox/shardseal/naming_test.go`
- 测试：`pkg/cryptox/shardseal/shardseal_test.go`（`TestMetaNameStructure`、`TestEncryptShards_NamingConvention` 适配）

- [ ] **步骤 1：编写失败的测试**

在 `naming_test.go` 新增（沿用 table/`t.Parallel()` 风格）：

```go
func TestDirMetaNameStructure(t *testing.T) {
	t.Parallel()
	name := DirMetaName("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	if !strings.Contains(name, "@") {
		t.Errorf("目录 meta 名应含 @ 标记：%q", name)
	}
	if !IsDirMetaName(name) {
		t.Errorf("IsDirMetaName(%q) 应为 true", name)
	}
	if ClassifyName(name) != KindDirMeta {
		t.Errorf("ClassifyName(%q)=%v，want KindDirMeta", name, ClassifyName(name))
	}
}

func TestRandDirName(t *testing.T) {
	t.Parallel()
	for range 100 {
		n, err := RandDirName()
		if err != nil {
			t.Fatalf("RandDirName: %v", err)
		}
		if len(n) < 5 || len(n) > 30 {
			t.Fatalf("目录名长度 %d 超出 5-30", len(n))
		}
		for _, c := range n {
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
				t.Fatalf("目录名含非 [a-z0-9] 字符 %q", c)
			}
		}
		if n == "meta" || n == "data" || n == "secret" || strings.HasPrefix(n, "secret") {
			t.Fatalf("目录名命中保留词：%q", n)
		}
	}
}

func TestClassifyName(t *testing.T) {
	t.Parallel()
	cases := map[string]NameKind{
		"0123456789abcdefAbC": KindChunk,
		"0123456789abcdef-x":  KindFileMeta,
		"0123456789abcdef_9":  KindFileMeta,
		"0123456789abcdef@9":  KindDirMeta,
	}
	for name, want := range cases {
		if got := ClassifyName(name); got != want {
			t.Errorf("ClassifyName(%q)=%v，want %v", name, got, want)
		}
	}
}

func TestNameLengthUniform(t *testing.T) {
	t.Parallel()
	// 三类文件名长度同分布 54-62：批量生成，断言三者 min/max 完全一致。
	build := func(fn func() string) (min, max int) {
		for range 200 {
			l := len(fn())
			if l < min || min == 0 {
				min = l
			}
			if l > max {
				max = l
			}
		}
		return min, max
	}
	o, t2, e := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"
	chunkMin, chunkMax := build(func() string { return ChunkName(o, t2, e) })
	fmMin, fmMax := build(func() string { return MetaName(o, t2, e) })
	dmMin, dmMax := build(func() string { return DirMetaName(o, t2, e) })
	if chunkMin != fmMin || chunkMin != dmMin || chunkMax != fmMax || chunkMax != dmMax {
		t.Errorf("三类文件名长度范围不一致：chunk %d-%d fileMeta %d-%d dirMeta %d-%d",
			chunkMin, chunkMax, fmMin, fmMax, dmMin, dmMax)
	}
	if chunkMin != 54 || chunkMax != 62 {
		t.Errorf("分块名长度 %d-%d，应为 54-62", chunkMin, chunkMax)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/cryptox/shardseal/ -run 'TestDirMetaNameStructure|TestRandDirName|TestClassifyName|TestNameLengthUniform'`
预期：FAIL（`DirMetaName`/`IsDirMetaName`/`ClassifyName`/`RandDirName`/`NameKind` 未定义）

- [ ] **步骤 3：实现 `pkg/cryptox/shardseal/naming.go` 扩展**

```go
// NameKind 是三文件名类型。
type NameKind int

const (
	KindChunk NameKind = iota // 无标记
	KindFileMeta              // 含 - 或 _
	KindDirMeta               // 含 @
)

// ClassifyName 按标记字符分类文件名（@ > -/_ > 无标记）。
func ClassifyName(name string) NameKind

// IsDirMetaName 报告是否目录 meta 名（含 @）。
func IsDirMetaName(name string) bool

// DirMetaName 构造目录 meta 文件名：{dirMeta原始前16hex}{rand1}{目录标识前16hex}{rand2}{dirMeta加密后前16hex}，
// rand 必含 @（目录 meta 识别标记）。长度与分块/文件 meta 一致（54-62）。
func DirMetaName(dirOrig, dirID, dirEnc string) string

// RandDirName 生成随机容器目录名（5-30 字符 [a-z0-9]；命中保留词 meta/data/secret 时重掷）。
func RandDirName() (string, error)
```

同时把 `MetaName(total)` 改为 `MetaName(metaOrig, total, metaEnc string)`——三段 hex 真实补齐（**去掉占位 `0000000000000000`**）：`{meta原始前16hex}{rand1}{原始总前16hex}{rand2}{meta加密后前16hex}`，rand 必含 `-`/`_`。语义：`metaOrig`=meta 明文 JSON 哈希前16、`total`=原始总校验和前16、`metaEnc`=meta 密文哈希前16（完整可靠）。保留 `IsMetaName`（含 `-`/`_`）供既有分类使用。

- [ ] **步骤 4：同步修 `MetaName` 调用点（`shardseal.go`）**

`EncryptShards` 里 `metaName := MetaName(totalHex)` 改为：marshal `metaJSON` 后算 `metaOrigHex := hash16(metaJSON)`，本任务阶段 meta 未加密（`metaEncHex` 暂用 `metaOrigHex` 占位，任务 3 换真实密文哈希）：
`metaName := MetaName(metaOrigHex, totalHex, metaOrigHex)`

- [ ] **步骤 5：运行测试验证通过**

运行：`go test ./pkg/cryptox/shardseal/`
预期：PASS（`TestMetaNameStructure` 与 `TestEncryptShards_NamingConvention` 若因签名变化编译失败，同步改为三参调用并保持原断言语义）

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/naming.go pkg/cryptox/shardseal/naming_test.go pkg/cryptox/shardseal/shardseal.go pkg/cryptox/shardseal/shardseal_test.go
git commit -m "feat(shardseal): 命名扩展——目录 meta @ 标记、随机容器目录名 5-30、MetaName 三段真实补齐、三类文件名长度锁定 54-62"
```

---

### 任务 2：crypto 统一落盘格式（R 随机首部 + 4B 长度头 + meta 加密）

**文件：**
- 修改：`pkg/cryptox/shardseal/crypto.go`
- 测试：`pkg/cryptox/shardseal/crypto_test.go`（新增）

- [ ] **步骤 1：编写失败的测试**

新增 `crypto_test.go`（`t.Parallel()`；小块策略便于测试）：

```go
func TestEncryptBlock_UniformOnDiskFormat(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	salt := bytes.Repeat([]byte{0x22}, SaltLen)
	blob, err := encryptBlock(key, salt, []byte("hello"))
	if err != nil {
		t.Fatalf("encryptBlock: %v", err)
	}
	// 格式 [R 128B][4B 密文长][salt][nonce][ct+tag]
	if len(blob) != RandPrefixLen+4+SaltLen+NonceLen+len("hello")+16 {
		t.Errorf("blob 长度 %d 不符 R+4+salt+nonce+ct+tag", len(blob))
	}
	// 长度头 = 密文长度 = len(blob) - R - 4 - 32 - 12
	ctLen := binary.BigEndian.Uint32(blob[RandPrefixLen : RandPrefixLen+4])
	if int(ctLen) != len(blob)-RandPrefixLen-4-SaltLen-NonceLen {
		t.Errorf("长度头 %d 与文件大小线性关系不符", ctLen)
	}
	// R 段内容随机（非全 0/全 1）
	if bytes.Equal(blob[:RandPrefixLen], bytes.Repeat([]byte{0}, RandPrefixLen)) {
		t.Error("R 段不应全零")
	}
	// 解密 roundtrip
	plain, err := decryptBlock(key, salt, blob)
	if err != nil || string(plain) != "hello" {
		t.Errorf("decryptBlock roundtrip: %v", err)
	}
	// 篡改 R 段不影响解密
	badR := append([]byte(nil), blob...)
	badR[0] ^= 0xFF
	if _, err := decryptBlock(key, salt, badR); err != nil {
		t.Errorf("篡改 R 段不应影响解密：%v", err)
	}
}

func TestEncryptMetaJSON_PadLengthInRange(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	salt := bytes.Repeat([]byte{0x44}, SaltLen)
	metaJSON := []byte(`{"version":1}`)
	// padTarget 传 0 = 不 padding（纯密文长度）；默认 pad 到目标
	blob, err := encryptMetaJSON(key, salt, metaJSON, 0)
	if err != nil {
		t.Fatalf("encryptMetaJSON: %v", err)
	}
	// 无 padding 时：明文 = [4B jsonLen][JSON]，密文长 = len(明文)+16
	plainLen := 4 + len(metaJSON)
	if len(blob) != RandPrefixLen+4+SaltLen+NonceLen+plainLen+16 {
		t.Errorf("无 padding blob 长度 %d 不符", len(blob))
	}
	// 解密精确还原（jsonLen 截取，padding 不进入 JSON）
	got, err := decryptMetaJSON(key, blob)
	if err != nil || string(got) != string(metaJSON) {
		t.Errorf("decryptMetaJSON: %v", err)
	}
	// 篡改密文段（padding 属 GCM 认证范围）必失败
	bad := append([]byte(nil), blob...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := decryptMetaJSON(key, bad); err == nil {
		t.Error("篡改密文应 fail-closed")
	}
}

func TestEncryptMetaJSON_PadToTarget(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x55}, KeyLen)
	salt := bytes.Repeat([]byte{0x66}, SaltLen)
	metaJSON := []byte(`{"a":"b"}`)
	const padTarget = 128
	blob, err := encryptMetaJSON(key, salt, metaJSON, padTarget)
	if err != nil {
		t.Fatalf("encryptMetaJSON: %v", err)
	}
	if len(blob) != padTarget {
		t.Errorf("padding 后 blob 长度 %d，应为目标 %d", len(blob), padTarget)
	}
	got, err := decryptMetaJSON(key, blob)
	if err != nil || string(got) != string(metaJSON) {
		t.Errorf("padding 后解密应还原原 JSON：%v", err)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/cryptox/shardseal/ -run 'TestEncryptBlock_UniformOnDiskFormat|TestEncryptMetaJSON'`
预期：FAIL（`RandPrefixLen`/`encryptMetaJSON`/`decryptMetaJSON` 未定义；`encryptBlock` 现无 R/长度头）

- [ ] **步骤 3：实现 `pkg/cryptox/shardseal/crypto.go`**

```go
// RandPrefixLen 是固定长度随机首部（R 段），每文件随机字节——首部无格式指纹。
const RandPrefixLen = 128
```

改造 `encryptBlock(key, salt, plain []byte) ([]byte, error)`：输出 `[R 128B crypto/rand][4B 密文长 BE][salt][nonce][ct+tag]`。`decryptBlock(key, expectSalt, blob []byte) ([]byte, error)`：跳 R → 读长度头 → 校验 salt → 取 nonce → GCM 解密；**篡改 R 段不影响**（只跳不校验）。

新增（meta 加密，格式与分块同构）：

```go
// encryptMetaJSON 加密 meta 明文：明文 = [4B jsonLen BE][metaJSON][rand padding]
// （padTarget>0 时 padding 到该总长；0 = 不 padding）。输出统一 [R][4B 密文长][salt][nonce][ct+tag]，
// 长度头与文件大小线性一致（padding 在密文内、属 GCM 认证范围）。
func encryptMetaJSON(key, salt, metaJSON []byte, padTarget int) ([]byte, error)

// decryptMetaJSON 解密 meta blob：跳 R → 长度头 → GCM → 读 4B jsonLen 截取真实 JSON。
func decryptMetaJSON(key, blob []byte) ([]byte, error)
```

说明：长度头 = 密文长度（`[salt][nonce][ct+tag]` 之后的 ct+tag 长度，即 `len(明文)+16`）；文件大小 = `R + 4 + 32 + 12 + len(密文)` 恒成立。派生 key 由调用方（shardseal.go）每文件一次 `deriveKey(secret, salt)`，本层不重复 scrypt。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/cryptox/shardseal/ -run 'TestEncryptBlock_UniformOnDiskFormat|TestEncryptMetaJSON'`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/crypto.go pkg/cryptox/shardseal/crypto_test.go
git commit -m "feat(shardseal): 统一落盘格式——R 随机首部 + 4B 长度头 + meta 加密（jsonLen+padding，密文内线性一致）"
```

---

### 任务 3：shardseal 适配新格式（EncryptShards 写密文 meta、DecryptFile 读新格式、padding 锁定）

**文件：**
- 修改：`pkg/cryptox/shardseal/shardseal.go`
- 修改：`pkg/cryptox/shardseal/meta.go`（`Options`/meta 无变化；如需 `MetaPadBytes` 放 `EncryptShards` 参数）
- 测试：`pkg/cryptox/shardseal/shardseal_test.go`

- [ ] **步骤 1：编写失败的测试**

在 `shardseal_test.go` 新增：

```go
// TestEncryptShards_MetaEncryptedOnDisk：meta 落盘是密文（非明文 JSON），
// 且带 R 首部 + 长度头线性一致；meta 名三段真实（首尾非全零占位）。
func TestEncryptShards_MetaEncryptedOnDisk(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("secret"), testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	metaBlob, err := os.ReadFile(filepath.Join(outDir, res.MetaName))
	if err != nil {
		t.Fatalf("读 meta: %v", err)
	}
	if bytes.Contains(metaBlob, []byte(`"version"`)) {
		t.Error("meta 落盘不应是明文 JSON")
	}
	// 长度断言：meta 明文 = [4B jsonLen][metaJSON]（padTarget=0 无 padding），密文长 = 明文+16
	metaJSON, _ := json.Marshal(res.Meta)
	want := RandPrefixLen + 4 + SaltLen + NonceLen + (4 + len(metaJSON)) + 16
	if len(metaBlob) != want {
		t.Errorf("meta 落盘长度 %d，应为 %d（R+长度头+salt+nonce+jsonLen+JSON+tag）", len(metaBlob), want)
	}
	// meta 名首尾段非全零（真实哈希）
	if strings.HasPrefix(res.MetaName, "0000000000000000") || strings.HasSuffix(res.MetaName, "0000000000000000") {
		t.Errorf("meta 名首尾段应为真实哈希，当前是占位：%q", res.MetaName)
	}
}
```

`TestDecryptFile_*` 既有测试继续跑（`DecryptFile` 读新格式分块 + 明文 `Meta` 结构——`res.Meta` 仍是明文，仅落盘密文）。

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/cryptox/shardseal/ -run TestEncryptShards_MetaEncryptedOnDisk`
预期：FAIL（meta 仍是明文落盘；长度不符）

- [ ] **步骤 3：实现 `shardseal.go` 适配**

`EncryptShards` 内：
1. 分块：`encryptBlock` 已返回新格式（任务 2），写盘不变；
2. meta：`json.Marshal(res.Meta)` → `metaOrigHex = hash16(metaJSON)` → `metaKey := deriveKey(secret, salt)` 复用文件 key → `metaBlob, _ := encryptMetaJSON(metaKey, salt, metaJSON, 0)`（`MetaPadBytes` 可选参数，默认 0=不 padding；secretdata 卷负责 padding 到分块范围，见任务 4）→ `metaName := MetaName(metaOrigHex, totalHex, hash16(metaBlob))`（真实三段）→ 写 `metaBlob` 到 `outDir/metaName`；
3. `DecryptFile`：分块读取用新 `decryptBlock`（内部跳 R/长度头），其余不变。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/cryptox/shardseal/`
预期：PASS（全部既有 + 新增）

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/shardseal.go pkg/cryptox/shardseal/shardseal_test.go
git commit -m "feat(shardseal): EncryptShards 落盘加密 meta（真实三段名）——磁盘 meta 不再明文泄漏"
```

---

### 任务 4：secretdata 布局重构（随机容器目录 + 目录 meta + 文件 meta 加密 + 覆盖写原子化）

**文件：**
- 修改：`pkg/volume/secretdata/secretdata.go`
- 修改：`pkg/volume/secretdata/helpers.go`
- 测试：`pkg/volume/secretdata/secretdata_test.go`

**背景：** 现有布局 `data/<hash16>/` + `meta/<hash16>/`（泄漏 data/meta 结构词与内容指纹）改为：逻辑目录 → 随机容器目录（5-30），容器内混放三类同构文件。文件 meta 只存 basename，目录 meta（@）存逻辑 path。

- [ ] **步骤 1：编写失败的测试**

重写/新增（保留既有 roundtrip/ListDir 语义，改底层布局断言）：

```go
// TestUnderlyingLayout_NoStructWords：底层根下不得出现 data/meta 目录；目录名随机 5-30。
func TestUnderlyingLayout_NoStructWords(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)

	rootEntries, err := fs.inner.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("inner ListDir: %v", err)
	}
	for _, e := range rootEntries {
		if e.Name == "data" || e.Name == "meta" {
			t.Errorf("底层不应出现 data/meta 结构目录：%q", e.Name)
		}
		if len(e.Name) < 5 || len(e.Name) > 30 {
			t.Errorf("容器目录名长度 %d 超出 5-30", len(e.Name))
		}
	}
}

// TestMetaFileSizeInRange（审查重点 2）：meta 落盘大小 ∈ [min_block_size, 2×min_block_size]
// （默认 [1MiB, 2MiB]），且与最小分块大小下限重叠——底层不得凭文件大小区分 meta 与分块。
// 注意：统一落盘格式含 R 首部，最小整块 = R128+4+32+12+16 = 192B；padTarget 语义 = 整块落盘
// 总长。newFS block policy Min=64 → 但 meta pad 目标须 ≥192（R 地板），故取 pad 目标 ∈ [192, 384]
// （即 [R 地板, 2×min_block_size 落盘]），测试断言 f.Size ∈ [192, 384]。
func TestMetaFileSizeInRange(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "m.bin", 300)

	// 读底层 meta blob 大小（容器内含 -/_ 标记的文件）
	rootEntries, _ := fs.inner.ListDir(ctx, "")
	container := rootEntries[0].Name
	inner, _ := fs.inner.ListDir(ctx, container)
	for _, f := range inner {
		if !shardseal.IsDirMetaName(f.Name) && shardseal.IsMetaName(f.Name) {
			if f.Size < 192 || f.Size > 384 {
				t.Errorf("meta 文件大小 %d 不在 [192, 384]（R 地板 192 起）范围", f.Size)
			}
		}
	}
}

// TestUnderlyingLayout_DirMetaMarked：容器内混放三类文件，目录 meta 含 @。
func TestUnderlyingLayout_DirMetaMarked(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)

	rootEntries, _ := fs.inner.ListDir(ctx, "")
	container := rootEntries[0].Name
	inner, _ := fs.inner.ListDir(ctx, container)
	hasDirMeta, hasFileMeta, hasChunk := false, false, false
	for _, f := range inner {
		switch {
		case shardseal.IsDirMetaName(f.Name):
			hasDirMeta = true
		case shardseal.IsMetaName(f.Name):
			hasFileMeta = true
		default:
			hasChunk = true
		}
	}
	if !hasDirMeta || !hasFileMeta || !hasChunk {
		t.Errorf("容器应含目录meta(@)/文件meta(-)/分块三类，got dirMeta=%v fileMeta=%v chunk=%v", hasDirMeta, hasFileMeta, hasChunk)
	}
}

// TestOverwrite_Atomic：覆盖写新随机名先传后删旧——中途失败旧数据完好（F-2）。
// v3 模型：容器 = 逻辑目录（不随文件覆盖改变）；覆盖写在容器内以**新随机 meta blob 名**
// 上传新版本，成功后索引切换到新条目并 best-effort 删除旧 meta blob + 旧分块。
func TestOverwrite_Atomic(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "ov.bin", 800)
	oldEntry := fs.index["ov.bin"]
	oldContainer := oldEntry.dirSeg
	oldMeta := oldEntry.metaName

	writeContent(t, fs, ctx, "ov.bin", 1000)
	newEntry := fs.index["ov.bin"]
	if newEntry.dirSeg != oldContainer {
		t.Error("覆盖写不应更换容器目录（容器=逻辑目录）")
	}
	if newEntry.metaName == oldMeta {
		t.Error("覆盖写应生成新随机 meta blob 名，而非原地覆盖")
	}
	// 旧 meta blob 应被清理（孤儿残留禁止）
	if ent, _ := fs.inner.Stat(ctx, path.Join(oldContainer, oldMeta)); ent != nil {
		t.Error("覆盖写后旧 meta blob 应被删除（孤儿残留）")
	}
	rc, _ := fs.OpenRead(ctx, "ov.bin")
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(1000)) {
		t.Error("覆盖写后应读到新版本")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/volume/secretdata/ -run 'TestUnderlyingLayout_NoStructWords|TestUnderlyingLayout_DirMetaMarked|TestOverwrite_Atomic'`
预期：FAIL（布局仍是 data/meta；`metaEntry` 无 `dirSeg`/`metaPath` 字段）

- [ ] **步骤 3：重构 `secretdata.go` 数据结构与写入路径**

`SecretdataFS` 字段：`index map[string]*metaEntry`（键 = 完整逻辑 rel）；`dirs map[string]struct{}`（逻辑目录）；`dirSegs map[string]string`（**逻辑目录 → 随机容器目录名**，写路径定位/创建容器用）；`metaEntry` 改 `{size, mtime, dirSeg, metaName, meta *shardseal.Meta}`。

`writeFile(ctx, rel, r, size, mtime)`：
1. `io.ReadAll` 明文 → `encryptContent` 分块加密（新格式）到临时目录；
2. `parentDir := path.Dir(rel)`；容器 = `dirSegs[parentDir]`，不存在则 `RandDirName()` 创建 + 上传目录 meta（@，加密 JSON `{type:"dir", path:parentDir}`）→ 登记 `dirSegs`/`dirs`；
3. 上传全部分块 + **加密 meta blob**（`encryptMetaJSON`，`meta_pad_bytes` 目标，默认 `min_block_size`）到容器；
4. **索引原子切换**：全部上传成功后才 `s.index[rel] = newEntry` + 更新 `dirs`；旧版本条目记录旧容器/旧 meta 名，切换后 best-effort 删旧 meta + 旧分块（**失败路径：新容器/新 blob 上传中途失败 → 删新留旧**）。

`encryptContent`（helpers.go）保持返回 `*shardseal.EncryptionResult`（含明文 `Meta` + `MetaName`）不变；**加密 meta blob 由 writeFile 生成**：`json.Marshal(out.Meta)` → `encryptMetaJSON(key, salt, metaJSON, padTarget)` → 以 `out.MetaName`（真实三段）为名上传。`sanitizeName` 仅用于临时源文件（`filepath.Base`），与 shardseal 写入 meta 的 `Original.Name`（basename）一致。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/volume/secretdata/`
预期：PASS（`TestWriteReadRoundtrip`/`TestWrite_ThenStatAndList`/`TestDelete`/`TestListDir_ShowsSubdirectories` 若断言旧布局/旧字段需同步适配；`TestLoadIndexFromExistingVolume` 在新布局下继续成立）

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/secretdata/secretdata.go pkg/volume/secretdata/helpers.go pkg/volume/secretdata/secretdata_test.go
git commit -m "feat(secretdata): 布局重构——随机容器目录 + 目录meta(@) + 加密文件meta，覆盖写原子化（F-2）"
```

---

### 任务 5：secretdata 读取/索引恢复/目录语义（openRead + loadIndex + MakeDir/移动）

**文件：**
- 修改：`pkg/volume/secretdata/secretdata.go`
- 测试：`pkg/volume/secretdata/secretdata_test.go`

- [ ] **步骤 1：编写失败的测试**

新增：

```go
// TestLoadIndex_RestoresDirTree：重启后从目录meta.path+文件meta.basename重建完整路径树
// （修复 F-1：子目录不丢失、不同目录同名文件不冲突）。
func TestLoadIndex_RestoresDirTree(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a.bin", 200)
	writeContent(t, fs, ctx, "movies/sub/f1.mp4", 300)
	writeContent(t, fs, ctx, "docs/sub/f1.mp4", 100) // 同名 basename 不同目录

	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	assertListDir(t, fs2, ctx, "", map[string]bool{"a.bin": false, "movies": true, "docs": true})
	assertListDir(t, fs2, ctx, "movies/sub", map[string]bool{"f1.mp4": false})
	assertListDir(t, fs2, ctx, "docs/sub", map[string]bool{"f1.mp4": false})
	// 两个同名文件都能读回各自内容
	rc, _ := fs2.OpenRead(ctx, "movies/sub/f1.mp4")
	m1, _ := io.ReadAll(rc)
	rc.Close()
	rc2, _ := fs2.OpenRead(ctx, "docs/sub/f1.mp4")
	m2, _ := io.ReadAll(rc2)
	rc2.Close()
	if bytes.Equal(m1, m2) {
		t.Error("不同目录同名文件应读出不同内容")
	}
}

// TestDirMove_UpdatesMetaOnly：目录移动仅更新目录meta.path，文件可解析（审查重点5）。
func TestDirMove_UpdatesMetaOnly(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "old/f1.mp4", 200)
	oldDir := fs.index["old/f1.mp4"].dirSeg
	oldMeta := fs.index["old/f1.mp4"].metaName
	if err := fs.Rename(ctx, "old", "new"); err != nil {
		t.Fatalf("Rename 目录: %v", err)
	}
	if fs.index["old/f1.mp4"].dirSeg != oldDir || fs.index["old/f1.mp4"].metaName != oldMeta {
		t.Error("目录移动不应改动文件 meta/分块")
	}
	if !fs.dirs["new"] {
		t.Error("移动后新目录应存在")
	}
	assertListDir(t, fs, ctx, "new", map[string]bool{"f1.mp4": false})
	if _, err := fs.Stat(ctx, "old"); err != nil || fs.dirs["old"] {
		t.Error("移动后旧目录不应存在")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/volume/secretdata/ -run 'TestLoadIndex_RestoresDirTree|TestDirMove_UpdatesMetaOnly'`
预期：FAIL（loadIndex 仍按旧 meta/ 布局扫描；Rename 返回「暂不支持」）

- [ ] **步骤 3：实现读取/恢复/目录语义**

1. **openRead**：`index[rel]` → 容器 `dirSeg` 读加密 meta blob → `decryptMetaJSON` → `Meta` → 读分块（新格式 `decryptBlock`）到临时目录 → `shardseal.DecryptFile`（整文件 SHA-256 校验 fail-closed）；
2. **loadIndex**：扫根下所有容器目录 → 内部分类：`@`=目录 meta（解密 → path → `dirSegs[path]=容器名`、`dirs` 登记）；`-`/`_`=文件 meta（解密 → basename → `index[path.Join(dirMetaPath, basename)]` 建条目，**目录 meta.path + basename 作键**）；无标记=分块忽略。目录 meta 缺失但含文件 meta 的容器 → 视为根下文件（path=""）或跳过（fail-closed 选：跳过并记日志）；
3. **MakeDir**：逻辑目录 = 创建随机容器 + 写目录 meta（@，path=rel），登记 `dirs`/`dirSegs`——**不转发明文目录**；
4. **Rename**：若 `from` 是目录（`dirs[from]`）：改该容器目录 meta 的 `path` 为 `to`（递归：所有以 `from/` 为前缀的 dirs 条目 path 前缀替换），`index`/`dirSegs` 键同步迁移；文件 Rename（`index[from]` 存在）→ 改为更新所属容器 meta 下 basename 不变、`index` 键迁到 `to`（逻辑路径变化，容器不变）。失败路径不落半态（先校验 `to` 不存在）。
5. **Delete**：删文件 meta + 分块；容器清空（仅剩目录 meta）→ 保留容器（空目录），`pruneDirsLocked` 清理不再被引用的目录（目录 meta 同步删）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/volume/secretdata/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/secretdata/secretdata.go pkg/volume/secretdata/secretdata_test.go
git commit -m "feat(secretdata): loadIndex 按容器+@/-分类恢复逻辑路径树（F-1）、目录移动仅改目录meta、MakeDir/Rename/Delete 对齐新布局"
```

---

### 任务 6：装配适配（secret_register.go + root.go）+ 全量验证

**文件：**
- 修改：`cmd/sproxy/secret_register.go`
- 修改：`cmd/sproxy/root.go`（若 `Options` 新增字段需装配传参）
- 测试：`cmd/sproxy/secret_register_test.go`、`cmd/sproxy/secret_register_helper_test.go`

- [ ] **步骤 1：适配装配**

`secretdata.Options` 若新增 `MetaPadBytes int64`（`extra.meta_pad_bytes`，默认 = `Block.Min`）：`vcExtraBlockPolicy` 旁增解析，`registerSecretdataBackendWithFS` 传入。`setupSecretBackends` 不变。检查 `secret_register_test.go` 现有用例仍通过。

- [ ] **步骤 2：运行测试验证通过**

运行：`go test ./cmd/sproxy/ -run 'Secret|Secretdata|SetupSecret'`
预期：PASS

- [ ] **步骤 3：全量验证（门禁）**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/... ./pkg/volume/secrets/... ./cmd/sproxy/
go vet ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
golangci-lint run ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/... ./cmd/sproxy/...
go fmt ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/... ./cmd/sproxy/
```

预期：全绿；lint 0 issues；`go test ./...` 无回归。

- [ ] **步骤 4：Commit**

```bash
git add cmd/sproxy/secret_register.go cmd/sproxy/root.go cmd/sproxy/secret_register_test.go cmd/sproxy/secret_register_helper_test.go
git commit -m "feat(secretdata): 装配 meta_pad_bytes 配置 + 全量验证通过"
```

---

### 任务 6b：收敛 meta 生成（方案 B——EncryptShards 支持 padding + 返回最终 blob）

**背景/裁定：** 任务 4 曾为「名称锚定内容指纹」打补丁（I1：文件 meta 不复用 out.MetaName、现算三段哈希），根因是 `EncryptShards` 把 padTarget 写死为 `0`，逼 secretdata 重新加密（二次 GCM + 废弃中间 blob_A）。用户裁定采纳方案 B：**shardseal 一次生成最终 meta blob + 锚定它的名字**，secretdata 直接上传，消除二次加密与概念混淆。

**文件：**
- 修改：`pkg/cryptox/shardseal/shardseal.go`
- 修改：`pkg/cryptox/shardseal/shardseal_test.go`
- 修改：`pkg/volume/secretdata/secretdata.go`
- 修改：`pkg/volume/secretdata/helpers.go`
- 测试：`pkg/volume/secretdata/secretdata_test.go`

- [ ] **步骤 1：改 `EncryptShards` 签名 + `EncryptionResult`**

```go
// EncryptShards 把本地文件加密为分块 + meta，返回分块与 meta 信息。
// padTarget 为 meta 加密 padding 目标（0=不 padding；secretdata 卷传 min_block_size 附近值）。
func EncryptShards(srcFile, outDir string, secret []byte, policy BlockPolicy, padTarget int) (*EncryptionResult, error)

// EncryptionResult 返回加密产物（分块文件名列表 + 最终 meta blob + meta 文件名 + meta 明文）。
type EncryptionResult struct {
    ChunkNames []string
    MetaName   string   // 三段哈希锚定 MetaBlob（metaOrig=hash16(metaJSON)、total、metaEnc=hash16(MetaBlob)）
    MetaBlob   []byte   // 最终可上传的加密 meta（含 padding）
    Meta       *Meta    // 明文结构（内存索引 / DecryptFile 用）
}
```

`EncryptShards` 内：`metaBlob := encryptMetaJSON(metaKey, salt, metaJSON, padTarget)` → `metaName := MetaName(metaOrigHex, totalHex, hash16(metaBlob))` → 写 `metaBlob` 到 `outDir/metaName` → `res.MetaBlob = metaBlob`。

- [ ] **步骤 2：更新 shardseal 测试**

`shardseal_test.go` 调用点加 `padTarget=0`；`TestEncryptShards_MetaEncryptedOnDisk` 改断言 `res.MetaBlob` 直接存在且 `hash16(res.MetaBlob)` 为 `res.MetaName` 末段（不再从磁盘重读）。

- [ ] **步骤 3：secretdata writeFile 收敛**

`writeFile` 改：`out, _ := encryptContent(...)`（`encryptContent` 透传 `padTarget`）→ **直接上传 `out.MetaBlob` + `out.MetaName`**，删除任务 4 的重新加密 + 现算逻辑（`buildMetaBlob` 冗余，删除）；`encryptContent` 签名加 `padTarget int` 透传给 `EncryptShards`。

- [ ] **步骤 4：目录 meta 对齐同一模式**

目录 meta 也收敛：`ensureContainer` 用 `encryptMetaJSON(dmKey, salt, dmJSON, padTarget)` 一次生成 blob + `DirMetaName(Hash16(dmJSON), dirIDHex, Hash16(blob))`（现有已如此，仅确认 padTarget 语义一致、上传即最终 blob）。

- [ ] **步骤 5：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS（I1 修复的回归测试 `TestFileMetaNameHashSegmentsAlignBlob` 仍绿——现在名字天然锚定最终 blob，断言语义保持）

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/shardseal.go pkg/cryptox/shardseal/shardseal_test.go pkg/volume/secretdata/secretdata.go pkg/volume/secretdata/helpers.go pkg/volume/secretdata/secretdata_test.go
git commit -m "refactor(secretdata): EncryptShards 支持 meta padding + 返回最终 blob——消除二次加密与 out.MetaName 概念混淆（I1 结构性解法）"
```

---

### 任务 6c：meta 记录分块偏移量（为随机访问准备）

**背景/裁定：** 用户（2026-10-02）新增需求——meta 已记录 chunk 文件大小（orig_size/enc_size），再把 **offset**（分块在原始文件中的字节偏移）直接算好存进 meta，便于将来实现随机访问（给定文件内偏移 → 定位到所属分块 → 只下载/解密该分块）。设计文档 §5.1 已更新 chunks schema（新增 `offset` 字段）。

**文件：**
- 修改：`pkg/cryptox/shardseal/meta.go`（`ChunkInfo` 加 `Offset` 字段）
- 修改：`pkg/cryptox/shardseal/shardseal.go`（`EncryptShards` 分块循环填 `Offset: b.Offset`）
- 测试：`pkg/cryptox/shardseal/shardseal_test.go`（`TestMeta_HasFullStat` 断言 offset 连续覆盖 [0, size)）

- [ ] **步骤 1：`meta.go` 的 `ChunkInfo` 加字段**

```go
// Offset 是分块在原始文件中的字节偏移（0-based；随机访问定位用）。
Offset int64 `json:"offset"`
```

- [ ] **步骤 2：`EncryptShards` 分块循环填充**

分块循环（`for _, b := range blocks`）的 `ChunkInfo` 字面量加 `Offset: b.Offset`（`Block{Offset,Size}` 已由 `plan.Plan` 产出，直接填入）。

- [ ] **步骤 3：测试断言**

`shardseal_test.go` 的 `TestMeta_HasFullStat`（或新增子断言）：遍历 `res.Meta.Chunks`，断言 `chunks[i].Offset == 前 i 块 OrigSize 累计`（即 offset 连续覆盖 [0, 文件总大小)）；`chunks[0].Offset == 0`；末块 `Offset+OrigSize == 文件大小`。

- [ ] **步骤 4：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS（offset 是新增字段、JSON 向后兼容——旧 meta 无 offset 字段解析为 0，随机访问只在新建 meta 上启用）

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/meta.go pkg/cryptox/shardseal/shardseal.go pkg/cryptox/shardseal/shardseal_test.go
git commit -m "feat(shardseal): meta 记录分块偏移 offset——为随机访问定位分块做准备"
```

---

### 任务 7：blob 自描述契约 + 低成本清理

**背景/裁定：** 用户（2026-10-02 方案 B）需求 3：拿到一个块直接解密、不依赖 meta，meta 作为索引加速。分块 blob 已自包含（salt/nonce 内嵌固定偏移），缺**显式独立解密 API + 测试**。最终审查 M1/M3/M5/M8 低成本清理随本任务落地。

**文件：**
- 修改：`pkg/cryptox/shardseal/meta_blob.go`
- 修改：`pkg/cryptox/shardseal/naming.go`（M1 恒真守卫注释纠偏）
- 修改：`pkg/cryptox/shardseal/meta.go`（M3 删除 `Meta.MetaFileName` 死字段）
- 修改：`cmd/sproxy/secret_register.go`（M8 删 float32 死分支）
- 测试：`pkg/cryptox/shardseal/shardseal_test.go`、`cmd/sproxy/secret_register_test.go`

- [ ] **步骤 1：导出独立解密 API**

```go
// DecryptChunkStandalone 仅凭 secret + 分块 blob 独立解密（不依赖 meta）。
// blob 自描述：salt/nonce 内嵌固定偏移；按 blob 内嵌 salt 派生 key → GCM 解密。
// meta 中的 chunks（offset/size/sha256）只作索引加速与事后校验，不参与解密本身。
func DecryptChunkStandalone(secret, blob []byte) ([]byte, error)
```

复用 `MetaBlobSalt`/`DeriveKey`/`decryptBlock`（blob 内嵌 salt 作 expectSalt，自一致）。注意：当前 `decryptBlock` 的 `expectSalt` 语义 = blob 内嵌 salt（`meta_blob.go:37` `MetaBlobSalt` 已支持），独立解时传自身 salt 即自校验。

- [ ] **步骤 2：测试独立解密**

`shardseal_test.go` 新增 `TestDecryptChunkStandalone`：`EncryptShards` 产分块 → 取第一个分块 blob → `DecryptChunkStandalone(secret, blob)` 还原明文 == 原块内容；错误密钥 fail-closed。

- [ ] **步骤 3：M 系列清理**

- M1：`naming.go` 恒真守卫（randCharset 不含 `-/_/@`，`containsMetaMark(r1)&&containsMetaMark(r2)` 恒 false）——改为显式注释「rand 集不含标记，注入分支恒真为设计」，或直接注入（不再守卫）。
- M3：`meta.go` 删除 `Meta.MetaFileName` 字段（omitempty、任务 3 移除回填后无赋值源、永不会序列化——删除是安全改动）。
- M5：`hash16`/`Hash16` 返回错误被 `_` 丢弃的调用点——hash16 恒返回 nil err，改为无错误签名或集中注释说明。
- M8：`secret_register.go` `vcExtraInt64` 删 `float32` 分支（Go JSON/YAML 数字恒 float64）。

- [ ] **步骤 4：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/... ./cmd/sproxy/
```

预期：PASS（删除死字段/死分支不改行为）

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/ cmd/sproxy/secret_register.go
git commit -m "feat(shardseal): 分块独立解密 API（不依赖 meta）+ M1/M3/M5/M8 清理"
```

---

### 任务 8：算法域分离（预留算法类型，不明文避免特征）

**背景/裁定：** 用户（2026-10-02 方案 B）需求 2：预留算法类型、blob 不明文算法标识避免特征，便于后续自由扩展。当前 `deriveKey(secret, salt)` 无算法版本，blob 格式无算法标识——解密算法硬编码 AES-256-GCM。设计：算法版本经 **KDF 派生域**混入（version 进 scrypt 派生输入），blob 零明文算法特征；meta.algorithm（密文内）作权威；注册表按版本试派生。

**文件：**
- 修改：`pkg/cryptox/shardseal/crypto.go`
- 修改：`pkg/cryptox/shardseal/meta_blob.go`
- 修改：`pkg/cryptox/shardseal/meta.go`
- 修改：`pkg/cryptox/shardseal/shardseal.go`
- 修改：`pkg/volume/secretdata/secretdata.go`（`decryptBlob` 按 meta.algorithm 选版本）
- 测试：`pkg/cryptox/shardseal/crypto_test.go`、`shardseal_test.go`

- [ ] **步骤 1：定义算法版本与注册表**

```go
// AlgoVersion 是加密算法版本（KDF 派生域；blob 内不明文存储，仅影响派生结果）。
type AlgoVersion byte

const (
    AlgoV1GCM AlgoVersion = 1 // AES-256-GCM（当前唯一实现）
)

// Algorithm 是注册的算法定义：版本 → 派生域标记 + 加密函数。
type Algorithm struct {
    Version   AlgoVersion
    KDFDomain string // scrypt 派生域标记（混入 secret，域不同 key 不同）
    // Encrypt/Decrypt 工厂：本 PR 仅 AES-256-GCM，函数签名先定义为统一形式。
    Encrypt func(key, salt, plain []byte) ([]byte, error)
    Decrypt func(key, salt, blob []byte) ([]byte, error)
}

// RegisterAlgorithm 注册算法（装配期；重复版本 fail-fast panic）。
func RegisterAlgorithm(a Algorithm)
```

`deriveKey(secret, salt, v AlgoVersion)`：派生输入 = `secret || kdfDomain(v)`（版本域混入 secret，**不明文进 blob**）；v1 域 = `"shardseal/v1"` 显式标记（**无兼容垫**——用户硬约束：未上线无需兼容，旧 blob 不存在）。

- [ ] **步骤 2：meta 记录算法版本（密文内）**

`Meta.Algorithm` 已有（`"shardseal/aes-256-gcm"` 字符串）；新增 `Meta.AlgoVersion AlgoVersion`（json `algo_version`）供解密选版本。meta 加密落盘——版本在密文内、底层不可见。

- [ ] **步骤 3：解密按版本选派生**

`secretdata.decryptBlob` 读 meta（或独立解时）→ 按 `AlgoVersion` 调 `deriveKey(secret, salt, v)` → GCM 解密。blob 独立解（任务 7 `DecryptChunkStandalone`）改为**按注册表试所有版本**（版本数少，试派生成本 = 版本数 × scrypt；后续可加缓存）。

- [ ] **步骤 4：测试**

`TestDeriveKey_VersionDomainSeparation`：同 secret+salt 不同版本派生不同 key；v1 域 = `"shardseal/v1"`（无兼容断言——未上线，无旧行为可比）。
`TestDecrypt_ByMetaVersion`：meta.algo_version=1 解密成功；未知版本 fail-closed。
`TestDecryptChunkStandalone_AllVersions`：独立解遍历注册表试派生成功。

- [ ] **步骤 5：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/
git commit -m "feat(shardseal): 算法版本 KDF 域分离 + 注册表——预留算法扩展，blob 零明文算法特征"
```

---

### 任务 9：blocklet 细分（视频关键帧随机访问）

**背景/裁定：** 用户（2026-10-02 方案 B）需求 1：块内部支持细分块容器，更好支持随机访问（面向视频关键帧），仅加载块的部分内容即可解密。当前块 1MB-200MB 不再细分，随机访问一个位置要下载并解密整个块。设计：块 blob 内 **blocklet 序列**，各 blocklet 自描述（内嵌 offset/len/nonce）可独立解密；`BlockPlanner` 预留 `video-keyframe`（关键帧边界规划）。

**文件：**
- 修改：`pkg/cryptox/shardseal/block.go`（BlockletPlanner 两层规划）
- 修改：`pkg/cryptox/shardseal/crypto.go`（encryptBlocklets）
- 修改：`pkg/cryptox/shardseal/meta.go`（ChunkInfo 加 Blocklets）
- 修改：`pkg/cryptox/shardseal/shardseal.go`（EncryptShards/DecryptFile 适配两层）
- 修改：`pkg/cryptox/shardseal/naming.go`（blocklet 命名）
- 修改：`pkg/volume/secretdata/secretdata.go`（OpenRangeRead 随机读取）
- 测试：`pkg/cryptox/shardseal/*_test.go`

- [ ] **步骤 1：blocklet 规划接口**

```go
// BlockletPlanner 规划单块的 blocklet 序列（关键帧边界 / 固定大小）。
type BlockletPlanner interface {
    PlanBlocklets(origSize, blockOffset, blockSize int64) ([]Blocklet, error)
}

// Blocklet 是块内子块描述。
type Blocklet struct {
    Offset int64 // blocklet 在原始文件中的字节偏移
    Size   int64 // blocklet 原始明文大小
}
```

`BlockPolicy` 加 `BlockletMin/BlockletMax`（默认 64KB-4MB）与 `BlockletMode`（"fixed" 默认 | "video-keyframe" 预留）；`Planner()` 返回块 + blocklet 双层规划器。

- [ ] **步骤 2：blocklet 加密 blob 格式**

块 blob 内 blocklet 序列（自描述、可独立解）：

```
[R][4B blocklet 数][salt][bl1: [4B off][4B len][nonce][ct+tag]]...
```

- 每个 blocklet 段：`[4B off BE][4B len BE][12B nonce][ciphertext+tag]`（off=原始偏移、len=明文长度，GCM 认证密文）；
- 块 blob 长度头语义保持（= 真实密文段总长）；
- 解密：读 R → 块长度头 → salt → 逐 blocklet 独立 GCM 解；**随机访问：按目标 offset 定位 blocklet 段 → 只解该段**。

`encryptBlock` 重构为 `encryptBlocklets(key, salt, blocklets []Blocklet, data []byte) ([]byte, error)`；`decryptBlockletAt(blob, targetOffset)` 定位并只解含该偏移的 blocklet。

- [ ] **步骤 3：meta 记录 blocklet 索引**

`ChunkInfo` 加 `Blocklets []BlockletInfo`：

```go
type BlockletInfo struct {
    Offset    int64 `json:"offset"`      // 原始偏移（0-based）
    Size      int64 `json:"size"`        // 原始明文大小
    EncSize   int64 `json:"enc_size"`    // 密文段大小（含 nonce+tag，定位段长用）
    OrigSHA256 string `json:"orig_sha256"`
}
```

`ChunkInfo.Offset/OrigSize` 保留（块级随机访问入口）；blocklet 粒度索引为随机访问加速。

- [ ] **步骤 4：DecryptFile 适配**

`DecryptFile` 全量还原：逐块 → 逐 blocklet GCM 解 → 拼接；整文件 SHA-256 校验保持（fail-closed）。
`openRead`（secretdata）新增随机读取：`OpenRangeRead(rel, offset, size)`——按 meta 定位块 → 定位 blocklet → 只下载/解密含目标范围的 blocklet 段。

- [ ] **步骤 5：测试**

`TestEncryptBlocklets_Roundtrip`：多 blocklet 块加解密还原。
`TestDecryptBlockletAt_RangeRead`：随机读取某 offset 范围，断言只返回该范围内容、内容正确。
`TestBlocklet_OffsetsCoverFile`：blocklet 偏移连续覆盖 [0, 块大小)。
`TestDecryptFile_BlockletFull`：全量还原仍正确（含 SHA-256 校验）。

- [ ] **步骤 6：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/
git commit -m "feat(shardseal): 块内 blocklet 细分 + 随机读取——视频关键帧随机访问基础"
```

---

### 任务 9b：meta 字段全量预留 + type 常量 + 设计文档 §13

**背景/裁定：** 用户（2026-10-02）指令：**预留所有**（12 项审计项全部计入后续发展规划）。本任务把全部 meta 字段与 type 常量预留固化（只加字段/常量，不写值不实现逻辑）+ 设计文档 §13 全景。

**文件：**
- 修改：`pkg/cryptox/shardseal/meta.go`（全量字段预留）
- 修改：`pkg/cryptox/shardseal/meta_blob.go`（extra 段读写辅助，若任务 9 未含）
- 修改：`docs/designs/2026-10-01-secret-volume.md`（新增 §13 架构演进预留全景）
- 测试：`pkg/cryptox/shardseal/meta_test.go`（JSON roundtrip 含新字段空/有值）

- [ ] **步骤 1：meta 字段全量预留**

`Meta` 加（全部密文内、omitempty，未来用、现在不写值）：

```go
// ---- 压缩（用户：加密/压缩解耦，改压缩算法不升级加密版本）----
Compressed bool `json:"compressed,omitempty"`
Compression string `json:"compression,omitempty"` // "none"/"zstd"（RegisterCompression 独立注册表）

// ---- 密钥池 / 轮换（用户：卷级数据互操作，同 secret 卷可互读）----
KeyID string `json:"key_id,omitempty"` // 派生时按 KeyID 从 secret 池选 secret

// ---- 扩展元数据（用户：文件名/大小/权限/备注/kv/原始校验和进密文，降外部 meta 依赖）----
Extra map[string][]byte `json:"extra,omitempty"`

// ---- 审计/溯源（2026-10-02 审计项 4/10）----
WriterID   string `json:"writer_id,omitempty"`    // 写入者指纹（PikPak 下载来源等）
SourceURL  string `json:"source_url,omitempty"`   // 溯源（下载来源）
AccessCount int64  `json:"access_count,omitempty"` // 访问计数（热数据统计）
LastAccess  string `json:"last_access,omitempty"` // 最近访问时间

// ---- 版本/乐观锁（审计项 6/12）----
BaseVersion int64  `json:"base_version,omitempty"` // 乐观锁 CAS 版本（多进程写前校验）
VersionSeq  int64  `json:"version_seq,omitempty"`  // 版本保留序号（覆盖写保留 N 个旧版本）
Supersedes  string `json:"supersedes,omitempty"`   // 被本版本取代的版本标识
VClock      string `json:"vclock,omitempty"`       // 版本时钟（防跨时区/时钟漂移覆盖误判）

// ---- 去重（审计项 7：块级内容寻址）----
RefCount int64 `json:"ref_count,omitempty"` // 块引用计数（去重共享）

// ---- 删除/墓碑（审计项 1：孤儿 GC）----
Deleted bool   `json:"deleted,omitempty"`   // 删除墓碑（loadIndex 跳过；GC 清理）
ExportedFrom string `json:"exported_from,omitempty"` // 备份/导出溯源

// ---- 安全（审计项 11：meta 独立签名）----
Signature string `json:"signature,omitempty"` // meta HMAC（HKDF 子域派生签名密钥）
```

- [ ] **步骤 2：type 常量全量预留（block.go）**

`block.go` 加（任务 9 已定义 Data/Padding/Extra/Boot/Index；本任务补齐）：

```go
// 块类型表（AAD 内、解密者可见；观察者不可见——边界保密）。
const (
    BlockletTypeData    BlockletType = 0x01 // 数据块
    BlockletTypePadding BlockletType = 0x02 // 空闲/未使用（padding 复用）
    BlockletTypeExtra   BlockletType = 0x03 // 附加数据段（size:data）
    BlockletTypeBoot    BlockletType = 0x0F // 引导段（索引块位置）
    BlockletTypeIndex   BlockletType = 0x10 // 索引块（段定位表）
    BlockletTypeRef     BlockletType = 0x12 // 去重引用块（引用其它 blob 段，不存新数据）
    BlockletTypeParity  BlockletType = 0x13 // 纠错块（XOR parity，k-of-k+1）
    // 0x11+ 预留未来（校验块/稀疏标记等）
)
```

- [ ] **步骤 3：设计文档 §13 架构演进预留全景**

`docs/designs/2026-10-01-secret-volume.md` 新增 §13：版本化机制（AlgoVersion 注册表 = 平滑演进唯一保证）；**12 项审计预留全景**（去重引用 ref/乐观锁/墓碑+GC/usage 记账/溯源/版本保留/流式/多副本/纠删码/访问计数/meta 签名/版本时钟）+ 多副本=XOR parity（k-of-k+1 纯 stdlib）+ Reed-Solomon 后续；对照类似实现差异（gocryptfs 流式/restic 去重压缩/age 多接收者/Tahoe 纠删码）。

- [ ] **步骤 4：测试**

meta JSON roundtrip 含全部新字段（空与有值）；type 常量表完整性（Data/Padding/Extra/Boot/Index/Ref/Parity 唯一且 0x10+ 预留）。

- [ ] **步骤 5：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS（纯字段/常量预留，无行为变更）

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/ docs/designs/
git commit -m "feat(shardseal): meta 全量字段预留（压缩/KeyID/去重/乐观锁/墓碑/溯源/签名）+ type 常量（Ref/Parity）+ §13 架构演进全景"
```

---

### 任务 9c：高优先 + 简单功能立刻实现

**背景/裁定：** 用户（2026-10-02 21:50）指令：高优先与简单功能**立刻实现**（非仅预留）——去重引用（RefCount 计数 + 同内容块共享）、乐观锁（BaseVersion 写前校验）、墓碑 + 孤儿 GC（Deleted 标记 + loadIndex 跳过 + 周期 GC）、usage 记账（卷级容量统计）、**blob mtime 打散（用户 22:23 定案）**。

**文件：**
- 修改：`pkg/cryptox/shardseal/meta.go`（字段已预留，本任务写值 + 语义）
- 修改：`pkg/volume/secretdata/secretdata.go`（Delete 写墓碑、loadIndex 跳过 deleted、usage 聚合、mtime 打散）
- 修改：`pkg/volume/secretdata/secretdata_test.go`
- 测试：`pkg/cryptox/shardseal/shardseal_test.go`

- [ ] **步骤 0：blob mtime 打散（用户 22:23 定案）**

`secretdata.Options` 加 `PreserveMTime bool`（装配 `extra.preserve_mtime`，默认 false）。**默认**：底层 blob 文件系统 mtime = **原始 mtime + 随机偏移**（如 0-48h 随机，所有分片/容器 meta 各自打散，防同文件分片时间聚类特征）；`PreserveMTime=true` 才透传原始 mtime（展示按时间排序用）。**逻辑层排序不受影响**（`metaEntry.mtime` 仍是原始值，`Stat`/`ListDir` 用它排序）。落点：`writeFile`/`uploadChunks`/`ensureContainer` 的 mtime 参数——默认应用 `scatterMTime(mtime)`，配置后原样传递。测试：`TestMTimeScatter_DefaultRandomized`（默认各 blob mtime 不同、不在原始值上聚类；`PreserveMTime=true` 时 == 原始值、逻辑层 Stat 恒为原始 mtime）。

- [ ] **步骤 1：去重引用（同内容块共享）**

`secretdata` 写入时计算块内容 SHA256 → 查询卷内已有 blob（hash 索引）→ 命中则 meta 引用（`ChunkInfo.RefFile`/`RefBlocklet` 或复用 `FileName` 指向已有 blob）+ `meta.RefCount++`；删除时 `RefCount--`，归零才物理删 blob。**同 secret 的卷天然可跨卷共享**（blob 自包含）。注意：独立 blob（每文件自包含）的当前模型不强制去重——本任务实现「可选去重」（配置 `dedup=true` 时启用），默认仍每文件独立（移动无关优先）。

- [ ] **步骤 2：乐观锁（多进程安全）**

`meta.BaseVersion` 写路径：`WriteFile` 前读旧 meta 的 BaseVersion → 写入时 CAS（`BaseVersion+1` 写新）；`Delete`/`Rename` 同理。`SecretdataFS` 加 `volVersion` 字段（卷级 base，随 loadIndex 初始化）。失败（版本冲突）返回明确错误。**这是多进程共享卷的并发安全基础**（当前单进程 `s.mu` 之上叠加乐观锁，跨进程仍安全）。

- [ ] **步骤 3：墓碑 + 孤儿 GC**

`Delete` 改为写墓碑：meta 标记 `Deleted=true` + 保留 blob（防并发读半态）→ `loadIndex` 跳过 Deleted 条目（不重建索引）→ **周期 GC**（后台 goroutine 或惰性触发）：扫容器内 blob vs meta 引用（含 Deleted 的引用可物理删）+ 无引用的孤立 blob → 删除。GC 触发点：`NewFS` 启动后 + 可配间隔。

- [ ] **步骤 4：usage 记账**

`SecretdataFS` 加 `usage` 字段（`map[string]int64` 或聚合值）：写入/删除时按 meta size 增减；`GET /api/stats`（或新 `Usage()` 方法）返回卷级容量。`loadIndex` 时按扫描 meta 累计（重启后正确）。

- [ ] **步骤 5：测试**

`TestDedup_SameContentSharedBlob`：同内容两文件 → 引用同一 blob、RefCount=2、删除一文件 RefCount=1、全删归零物理删。
`TestOptimisticLock_VersionConflict`：写前版本不一致 → 明确错误；一致 → 成功且 BaseVersion+1。
`TestTombstone_SkipOnReload`：Delete 后重启 loadIndex 不重建该文件；GC 后 blob 物理删除。
`TestUsage_Accumulates`：写/删后 usage 正确（含重启后 loadIndex 恢复）。

- [ ] **步骤 6：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS（-race 全绿——乐观锁 + GC 并发安全）

- [ ] **步骤 7：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/
git commit -m "feat(secretdata): 去重引用/乐观锁/墓碑+孤儿GC/usage 记账——高优先扩展落地"
```

---

### 任务 9d：多副本 + XOR 纠错（k-of-k+1，纯 stdlib）

**背景/裁定：** 用户（2026-10-02 21:50）指令：多副本/纠错码负担不大则实现。约束：**禁止第三方依赖**——Reed-Solomon（GF(2^8) 手写 ~千行）负担大记后续；**XOR parity（k-of-k+1，纯 stdlib ~100 行）立即实现**；多副本 = 容器复制到多底层卷（架构已支持自包含容器，写入时复制容器到全部 target）。

**文件：**
- 修改：`pkg/cryptox/shardseal/crypto.go`（XOR parity 编解码）
- 修改：`pkg/cryptox/shardseal/meta.go`（meta 加 Parity 段引用）
- 修改：`pkg/volume/secretdata/secretdata.go`（多 target 复制；Parity 段写入/恢复）
- 修改：`pkg/volume/secretdata/secretdata_test.go`
- 测试：`pkg/cryptox/shardseal/crypto_test.go`

- [ ] **步骤 1：XOR parity 编解码（纯 stdlib）**

```go
// XORParity 计算 k 个数据块的 XOR 奇偶校验块（k-of-k+1）。
// 恢复：任一数据块丢失 → parity XOR 其余 k-1 块。
func XORParity(blocks ...[]byte) ([]byte, error)

// RecoverFromParity 用 parity + k-1 个数据块恢复缺失块。
// missingIndex: 缺失块在原始顺序中的下标（0..k-1）。
func RecoverFromParity(parity []byte, blocks [][]byte, missingIndex int) ([]byte, error)
```

注意：parity 块**本身也加密**（同 blob 格式，type=0x13 Parity）；k 块与 parity 的映射记录在 meta（`ChunkInfo.ParityOf` / 组信息）。纯 XOR、无第三方依赖、数学正确性易验证（XOR 恒等式）。

- [ ] **步骤 2：多 target 复制（多副本）**

`secretdata.Options` 加 `Targets []string`（多个底层卷名）：写入时容器复制到全部 target（容器自包含、blob 不变）；读取时主 target 失败 → 从副本 target 读。`setupSecretBackends`/`secret_register.go` 装配多 target。**副本间一致性**：写全部成功才算成功（或 majority）；删除同 Multi 删。

- [ ] **步骤 3：Parity 段写入/恢复**

`EncryptShards`/secretdata 写入时（配置 `erasure=true`）为 k 个数据块生成 XOR parity（type=0x13）段，meta 记录映射；`OpenRead` 时目标块缺失（底层读失败）→ `RecoverFromParity` 恢复后解密。

- [ ] **步骤 4：测试**

`TestXORParity_Roundtrip`：k 块 + parity，每块缺失都能恢复。
`TestRecover_MissingBlock`：任一块缺失 → RecoverFromParity 还原 == 原块。
`TestMultiTarget_ReplicaRead`：主 target 删某 blob → 副本可读（移动无关验证）。
`TestParity_RecoverMissingBlock`：块丢失 → parity 恢复 → 解密成功。

- [ ] **步骤 5：运行测试验证通过**

```bash
go test ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
```

预期：PASS（-race 全绿）

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/
git commit -m "feat(secretdata): XOR 奇偶校验恢复 + 多 target 副本复制——数据冗余（k-of-k+1，纯 stdlib）"
```

---

### 任务 10：目录解耦（移动无关 + 目录间解耦 + Imp-1/2）

**背景/裁定：** 用户（2026-10-02 方案 B）需求 4/5：移动文件/目录不改内容（最理想仅移动相关文件即可用）；目录 meta 不关心父目录在哪、目录间完全解耦。当前目录 meta 存完整 `path: "news/2026"`——移动逻辑目录必须改写其 path（耦合）；文件 Rename 返回错误。设计：目录 meta 改存 `{name, parent_dir_id}`（父引用、根可空），逻辑路径沿 parent 链重算；移动子树仅改根节点 parent 引用，子树内零改动；loadIndex 重构 + 并行化（Imp-2）；空目录语义重新定义（Imp-1）。

**文件：**
- 修改：`pkg/volume/secretdata/secretdata.go`（大改：dirMeta 结构、loadIndex、Rename、pruneDirs、文件移动）
- 修改：`pkg/volume/secretdata/secretdata_test.go`
- 修改：`docs/designs/2026-10-01-secret-volume.md`（§5.2 目录 meta schema、§6.2 移动语义）

- [ ] **步骤 1：目录 meta 改父引用模型**

```go
type dirMeta struct {
    Version      int    `json:"version"`
    Algorithm    string `json:"algorithm"`
    KDF          string `json:"kdf"`
    Type         string `json:"type"` // "dir"
    Name         string `json:"name"` // 本目录逻辑名（不含路径）
    ParentDirID  string `json:"parent_dir_id,omitempty"` // 父目录 dir_id（根为空）
    DirID        string `json:"dir_id"` // 本目录唯一 ID（16 hex）
    MTime        string `json:"mtime"`
}
```

**逻辑路径解析**：沿 parent 链重算（根 → … → name）。`loadIndex` 建 `dir_id → dirMeta` 表，再沿链解析每个容器的逻辑路径 → 文件逻辑路径 = 该路径 + basename。

- [ ] **步骤 2：目录移动 = 改根 parent 引用**

`Rename(from, to)` 目录：找到 `from` 对应容器的 dirMeta → 改 `Name`（目录改名）或 `ParentDirID`（移动位置）→ **只重写该容器的目录 meta（一个文件）**；子树内其它目录/文件 blob 零改动。`to` 必须是已存在目录的 dir_id 或新的 Name。失败不落半态（先校验目标合法）。

- [ ] **步骤 3：文件移动 = 物理搬 blob + 索引迁移（零改内容）**

文件 Rename（跨目录移动，basename 不变）：文件 blob 自包含 → 从旧容器**物理复制**分块+文件 meta 到目标容器（blob 内容零改动）→ 删旧容器副本 → 更新内存索引。重启后 loadIndex 在新容器扫到该文件（逻辑路径 = 新容器路径 + basename）。文件改名（basename 变）仍返回错误（delete+write）。

- [ ] **步骤 4：空目录语义重新定义（Imp-1 修复）**

选 (a)：删除最后文件时连目录 meta + dirSegs 一起删（彻底成空、重启不复现）。`Delete` 后容器无文件 meta → 删除该容器目录 meta + 注销 dirSegs（pruneDirsLocked 同步删磁盘目录 meta）。补测试：删除最后文件后 ListDir/Stat 不再返回该目录、重启也不复现。

- [ ] **步骤 5：loadIndex 并行化（Imp-2 修复）**

`loadIndex` 按容器并行扫描（每容器 goroutine + WaitGroup），容器内文件 meta 并行解密；`s.mu` 粒度已按容器/文件细（loadContainer/loadContainerFileMeta 内部加锁），可安全并行。派生缓存（`(secret,salt) → key` LRU，小容量）缓解重复 scrypt。

- [ ] **步骤 6：测试**

`TestDirMove_UpdatesMetaOnly`（改造：断言仅改 parent 引用、子树零改动、重启可解析）。
`TestDirMove_SubtreeZeroTouch`：移动 `a` 整棵子树到新位置，断言 `a/child` 等子目录 meta/文件 blob 零改动（dirSeg/metaName 不变）。
`TestFileMove_PhysicalCopy`：文件跨目录移动，断言 blob 零改动（内容哈希不变）、重启后可解析。
`TestDeleteLastFile_RemovesDir`（Imp-1）：删最后文件后目录不现、重启不现。
`TestLoadIndex_Parallel`：多容器卷挂载成功（并行无竞态，-race 下绿）。

- [ ] **步骤 7：设计文档同步**

`docs/designs/2026-10-01-secret-volume.md` §5.2 目录 meta schema 更新（name+parent_dir_id）、§6.2 移动语义更新（改 parent 引用、文件物理搬移）、Imp-1 空目录语义（删干净）。

- [ ] **步骤 8：运行测试验证通过**

```bash
go test ./pkg/volume/secretdata/... ./pkg/cryptox/shardseal/... ./cmd/sproxy/
```

预期：PASS（-race 全绿）

- [ ] **步骤 9：Commit**

```bash
git add pkg/volume/secretdata/ docs/designs/
git commit -m "feat(secretdata): 目录父引用解耦——移动子树仅改根 parent、文件移动物理搬移零改内容、空目录删干净、loadIndex 并行化"
```

---

### 任务 11：DefaultExternal map 序修复（早期问题 P5）

**背景/裁定：** 早期审查问题 P5（确凿 bug，评分 75）：`registry/set.go` 的 `DefaultExternal` 用 Go map 随机迭代序取首个非 nil，注释却称「装配顺序决定默认」——Go map 迭代随机，多外部卷时默认卷不确定，`secrets://default/...` 可能落到未实现 URLResolver 的后端而 fail-closed。历史 PR #714 同类问题已在 master 修复过，此处需显式记录首个登记的外部卷。用户确认：高优先立即实现。

**文件：**
- 修改：`pkg/volume/registry/set.go`
- 测试：`pkg/volume/registry/set_test.go`

- [ ] **步骤 1：Set 显式记录首个外部卷**

`Set` 加 `firstExternal string` 字段（首个 `AddExternalVolume` 登记的卷名）；`AddExternalVolume` 首次时记录；`DefaultExternal` 改为按 `firstExternal` 查 `external[firstExternal]`（有则返回、无则按 map 遍历兜底）——**装配序确定性**，不再依赖 map 随机迭代。

- [ ] **步骤 2：测试**

`TestDefaultExternal_FirstRegisteredWins`：注册多外部卷（含一个未实现 URLResolver 的），断言 `DefaultExternal()` 恒返回首个登记卷（多次调用一致、非随机）；`AddExternalVolume` 首次记录、后续不覆盖。
`TestResolveURL_DefaultAuthority_Deterministic`：空/"default" authority 经 DefaultExternal 解析，多次调用同一结果。

- [ ] **步骤 3：运行测试验证通过**

```bash
go test ./pkg/volume/registry/... ./pkg/volume/secrets/... ./cmd/sproxy/
```

预期：PASS

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/registry/set.go pkg/volume/registry/set_test.go
git commit -m "fix(registry): DefaultExternal 显式记录首个外部卷——默认卷装配序确定，不再依赖 map 随机迭代"
```

---

### 任务 12：测试补充（边界场景 + 端到端，用户 09:02 要求）

**背景/裁定：** 用户要求新增测试覆盖、边界场景、端到端测试。现状：单测已较充分（106 个，错误密钥/range/tamper/parity/边界已有），但 **test/ 端到端无任何 secretdata 卷用例**（CLI 真流程未覆盖加密卷）。范围按方案 A 调整：验证「Delete 即时物理删 + 覆盖写即时回滚」确定性，去掉对后台 GC 并发守护的依赖。

**文件：**
- 修改：`test/e2e_cli_volumes_test.go`（或新建 `test/e2e_cli_secretdata_test.go`）
- 修改：`pkg/cryptox/shardseal/shardseal_test.go`、`pkg/volume/secretdata/secretdata_test.go`（边界补充）
- 复用：`test/e2e_cli_harness_test.go` 的 `startCLIEnv`/`sclientRun`/`sclientJSON`（自动注入签名凭据）

- [ ] **步骤 1：边界场景单测（补短板）**

- 空文件（0 字节）：加密/解密 roundtrip（若 RandomPlanner 拒绝则断言明确错误信息 + 文档标注已知边界）；
- 单 blocklet 块（文件 < blockletMin）边界；
- 块边界整字节对齐（size 恰为 blockletMin 倍数）；
- 超长文件名/路径（254+ 字节 basename、深层嵌套）——ValidateFilePath 边界；
- 截断 blob（R/长度头/boot/段各段截断）→ fail-closed；
- 篡改 AAD 段头/长度头/R 段 → fail-closed；
- 并发读写 stress（多 goroutine 同卷写/读/删，-race）——验证即时删/回滚确定性。

- [ ] **步骤 2：端到端（test/e2e，最大缺口）**

新建 `test/e2e_cli_secretdata_test.go`（`//go:build e2e`，复用 `startCLIEnv`）：
- config 含 `type: secrets` + `type: secretdata` 卷装配 → 启动成功（验证方案 A 后 Imp-2 修复——config 声明 secretdata 卷可装配）；
- `sclient trust login`（或 harness 注入凭据）→ `sclient upload` 到 secretdata 卷 → `sclient list`/`stat` 可见 → `sclient download` 内容一致；
- `sclient mv <dir>` 目录移动 → `sclient list` 新路径可见、旧路径消失；
- `sclient delete` → 验证即时物理删（磁盘容器内无残留 meta/分块）；
- 底层卷匿名性断言：根目录无 `data`/`meta` 结构词、容器目录名 5-30 随机。

- [ ] **步骤 3：跨包集成**

- secrets 卷 ↔ secretdata 卷嵌套（secret_url → secrets:// 解析 → 加密数据卷）；
- 多 target 副本复制 e2e（主/副本双 root 一致，写后两 root 都含容器）。

- [ ] **步骤 4：运行测试验证通过**

```bash
go test -race ./pkg/cryptox/shardseal/... ./pkg/volume/secretdata/...
go test -tags=e2e ./test/... -run Secretdata  # 端到端
```

预期：PASS（-race 全绿；e2e 真二进制流程）

- [ ] **步骤 5：Commit**

```bash
git add test/ pkg/cryptox/shardseal/ pkg/volume/secretdata/
git commit -m "test(secretdata): 边界场景 + CLI 端到端（即时删/目录移动/匿名性）+ 跨包集成覆盖"
```

---

### 任务 13：KDF 档位化（scrypt 参数进 Algorithm + high/standard/low 三档）

**背景/裁定：** 用户（2026-10-03）确认：secretdata 的 secret 是 256-bit 高熵随机（secrets.go:72-76），scrypt N=2^17（202ms/256MB）是「为低熵口令设计的交互式登录档」，对高熵密钥纯属过度防御。实测各档：2^17=202ms/256MB、2^14=25ms/32MB、2^12=6ms/8MB。决策：**scrypt 参数进 Algorithm（版本化），注册 high/standard/low 三档；默认 standard（2^14）；测试/低配用 low（2^12）**——根治测试资源爆炸（58 并行 × 256MB → ×8MB）、低配服务器可用、§13「KDF 参数显式化」预留兑现。

**文件：**
- 修改：`pkg/cryptox/shardseal/crypto.go`（Algorithm 加 ScryptN/R/P；deriveKey 从 alg 读；注册三档）
- 修改：`pkg/cryptox/shardseal/meta.go`（AlgorithmName 默认语义、validateMeta）
- 修改：`pkg/volume/secretdata/secretdata_test.go`（测试 helper 用 low 档）
- 修改：`pkg/volume/secretdata/helpers.go`（测试/装配传档）
- 修改：`cmd/sproxy/secret_register.go`（Algorithm 档位名解析）
- 修改：`docs/designs/2026-10-01-secret-volume.md`（§4.2/§13 KDF 档位）
- 测试：`pkg/cryptox/shardseal/crypto_test.go`

- [ ] **步骤 1：Algorithm 加 KDF 参数 + 注册三档**

```go
type Algorithm struct {
	Version   AlgoVersion
	Name      string
	KDFDomain string
	// ScryptN/R/P 是 KDF 强度档（版本化：不同档 = 不同 Version + 域分离）。
	ScryptN, ScryptR, ScryptP int
	Encrypt/Decrypt ...
}

// 三档（默认 standard 2^14；high 2^17 保守；low 2^12 测试/低配）：
//   "shardseal/aes-256-gcm"        → standard（N=2^14, r=8, p=1）
//   "shardseal/aes-256-gcm-high"   → N=2^17
//   "shardseal/aes-256-gcm-low"    → N=2^12
```

`deriveKey` 改从 `alg` 读 ScryptN/R/P（删除包级 `scryptN=1<<17` 常量；保留 KDFDomain 域分离）。init() 注册三档（各唯一 KDFDomain，如 "shardseal/v1"/"-high"/"-low"）。

- [ ] **步骤 2：默认 standard + 装配档位解析**

`AlgorithmName`（默认）→ standard；装配 `Options.Algorithm` 传档位名即可切换（secretdata 零绑定已支持）。config.example 注明三档。

- [ ] **步骤 3：测试用 low 档**

secretdata 测试 helper（newFS 等）`Algorithm: "shardseal/aes-256-gcm-low"`——单次派生 6ms/8MB，58 并行测试内存 ~几百 MB 可控（替代测试信号量方案，根治而非治标）。

- [ ] **步骤 4：测试 + 文档**

`TestDeriveKey_VersionDomainSeparation` 适配三档；新增档位 roundtrip（同档加密解密成功、跨档 fail-closed）；§4.2/§13 文档「KDF 档位 high/standard/low」+ 低档安全论证（256-bit 熵）。跑 `go test -race` 全绿。

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/ cmd/sproxy/ docs/designs/
git commit -m "feat(shardseal): KDF 档位化——scrypt 参数进 Algorithm（high/standard/low），默认 standard 2^14、测试/低配 low 2^12"
```

---

### 任务 14：test 档 KDF + CI 并行收敛（用户 18:17-18:25）

**背景/裁定：** 用户确认（2026-10-03）：CI 超时主因是 58 并行测试 × 2 核 runner × -race，非 scrypt 档位（本地多核 179s 主要是 FS 集成逻辑）。方案 A：**注册 test 档（N=2^8，~0.2ms/次）**——secretdata 测试用 test 档（仍走真实 scrypt 路径，KDF 集成覆盖保留），shardseal 包保持真实档（high/standard/low 正确性验证）；加 **-parallel 8**（make test/test-ci）根治 CI 2 核超时。

**文件：**
- 修改：`pkg/cryptox/shardseal/crypto.go`（注册 test 档 N=2^8）
- 修改：`pkg/volume/secretdata/secretdata_test.go`（测试 helper 用 test 档）
- 修改：`Makefile`（`test`/`test-ci` 加 `-parallel 8`）
- 修改：`docs/designs/2026-10-01-secret-volume.md`（§4.2 test 档注明）
- 测试：`pkg/cryptox/shardseal/crypto_test.go`

- [ ] **步骤 1：注册 test 档（N=2^8, r=8, p=1）**

`crypto.go` 加 `scryptNTest = 1 << 8` + 注册 `test` 档（Name "shardseal/aes-256-gcm-test"，域 "shardseal/v1-test"）——**真实 scrypt 路径、极低成本**（~0.2ms/次）。注释明确「test 档仅供测试/开发，生产禁配」。

- [ ] **步骤 2：secretdata 测试 helper 用 test 档**

`newFS`/`newFSSharedInner`/`newDedupFS`/`newErasureFS` 及内联 Options 构造统一 `testAlgo = "shardseal/aes-256-gcm-test"`（原 low 档 6ms → test 档 0.2ms，58 测试 ~300 次派生从 1.8s → 0.06s）。

- [ ] **步骤 3：Makefile 加 -parallel 8**

`test`/`test-ci` 目标追加 `-parallel 8`（全局测试并行上限——58 个 t.Parallel 最多 8 并发，CI 2 核可承受，本地多核不损失）。

- [ ] **步骤 4：测试 + 文档**

shardseal 包真实档测试不动（high/standard/low roundtrip/跨档 fail-closed 保留）；secretdata 测试 test 档后全绿。§4.2 注明 test 档仅供测试。跑 `go test -race` 全绿（secretdata 应从 19s 进一步降）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/ Makefile docs/designs/
git commit -m "feat(shardseal): test 档 KDF（N=2^8 极低档真实 scrypt）+ make test 加 -parallel 8——CI 2 核超时根治"
```

---

### 任务 15：移除 test 档 → 独立 mockkdf 子包（用户 19:37 根因重构）

**背景/裁定：** 用户根因重构：secretdata 是「组装编排」，不该跑真实重加密算法；算法可靠性由 shardseal 包负责。此前 test 档（N=2^8 低档真实 scrypt）是过渡产物——mock 派生（HKDF ~µs）优于任何低档 scrypt。**reset 移除 test 档，改为 `pkg/cryptox/shardseal/mockkdf` 子包**（供 secretdata 及其它依赖方复用），test 档不再是「荣誉功能」。

**文件：**
- 删除：`pkg/cryptox/shardseal/crypto.go` 的 `AlgoV1GCMTest`/`scryptNTest`/test 档注册（crypto.go:85/238-241）
- 新建：`pkg/cryptox/shardseal/mockkdf/mockkdf.go`（mock KDF 派生 + mock 算法注册，真实 AES-GCM 加密保留）
- 修改：`pkg/cryptox/shardseal/crypto.go`（Algorithm 加 `KDFOverride func(secret,salt []byte) ([]byte,error)`——nil=真实 scrypt；deriveKey 分支）
- 修改：`pkg/volume/secretdata/secretdata_test.go`（`testAlgo` 改 mockkdf 版本；high 两测保留真实 high 档）
- 修改：`docs/designs/2026-10-01-secret-volume.md`（§4.2 档位表移除 test 档、注明 mockkdf 子包）
- 测试：`pkg/cryptox/shardseal/mockkdf/mockkdf_test.go`

- [ ] **步骤 1：Algorithm 加 KDFOverride + deriveKey 分支**

`Algorithm` 加 `KDFOverride func(secret, salt []byte) ([]byte, error)`（nil = 真实 scrypt 默认；非 nil = 测试/开发注入 mock 派生）。`deriveKey`：`alg.KDFOverride != nil` → 用 override；否则真实 scrypt。

- [ ] **步骤 2：新建 mockkdf 子包**

`pkg/cryptox/shardseal/mockkdf/mockkdf.go`：提供 `MockKDF(secret, salt) ([]byte, error)` = **HKDF-SHA256**（secret 作 IKM、salt 作 info、固定标签）派生 32B key（~µs）；`RegisterMockAlgorithm()` 注册 `shardseal/aes-256-gcm-mock`（KDF=HKDF override、Encrypt/Decrypt=真实 AES-GCM——**加密组装仍真实验证**，仅派生轻量）。

- [ ] **步骤 3：移除 test 档**

crypto.go 删 `AlgoV1GCMTest`/`scryptNTest`/test 档 init 注册；`AlgoVersion` 常量重新编号（test 档曾占 4，移除后 high/standard/low = 1/2/3 不变——确认无既有数据依赖，未上线安全）。

- [ ] **步骤 4：secretdata 测试用 mockkdf**

`testAlgo = "shardseal/aes-256-gcm-mock"`；helper 需 `RegisterMockAlgorithm()`（once 守卫）。high 两测（TestLoadIndex_HighTier_*，测 loadGate 内存预算）保留真实 high 档——专门测「并发×派生内存守 512MiB」，必须真实。预期 secretdata 全包 150s → **~12s**（high 两测 8.7s + FS 编排几秒）。

- [ ] **步骤 5：测试 + 文档**

mockkdf_test（HKDF 派生确定性、mock 算法 roundtrip）；shardseal 真实档测试不动；secretdata mock 后全绿。§4.2 档位表移除 test 档、注明 mockkdf 子包「仅测试/开发，生产禁配」+ 分层说明（算法可靠性=shardseal 包，组装=secretdata mock）。跑 `go test -race` 全绿。

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/ pkg/volume/secretdata/ docs/designs/
git commit -m "refactor(shardseal): 移除 test 档，新增 mockkdf 子包——派生轻量 mock（HKDF）供依赖方测试，加密组装仍真实"
```

---

### 任务 16：功能文档 + 历史决策归档 + 包 README（用户 20:20-20:21）

**背景/裁定：** 用户要求：① 设计文档总结成**功能文档**（面向使用者）；② **历史决策归档**保留（方案反复过程）；③ 考虑给包放置 README.md（对齐 baidupcs 惯例）；④ **功能文档突出未来发展方向与预留**（§13）。

**产出文件：**
- 新建：`docs/secret-volume.md`（功能文档——能力/配置/用法/安全性/未来方向，面向使用者）
- 新建：`docs/designs/2026-10-01-secret-volume-decisions.md`（历史决策归档——方案反复的 WHY，维护者参考）
- 新建：`pkg/cryptox/shardseal/README.md`（算法包 README：能力/格式/档位/mockkdf/构建）
- 新建：`pkg/volume/secretdata/README.md`（卷包 README：能力/布局/目录解耦/即时删/GC/冗余/用法）
- 修改：`docs/designs/2026-10-01-secret-volume.md` 头部加指针「功能文档见 docs/secret-volume.md；历史决策见 ...decisions.md」（设计文档保留为规格详述）

- [ ] **步骤 1：功能文档 `docs/secret-volume.md`**

面向使用者（配置/CLI/API 用），章节：
1. 能力总览（加密算法/封装卷/匿名性/冗余）
2. 快速开始（config volumes[] 示例：type=secrets + type=secretdata + vault 示例）
3. 配置参考（Algorithm 档位 high/standard/low、block_policy、meta_pad_bytes、max_file_bytes、targets、gc_interval、preserve_mtime、erasure 实验性）
4. 寻址（secrets:// / secretdata://）+ 默认卷
5. 目录与移动语义（父引用解耦、移动零改内容）
6. 删除与 GC（即时物理删、GC 可选孤儿兜底）
7. 安全性（段边界保密、匿名性三维、meta 加密、KDF 档位论证）
8. **未来方向与预留（用户强调）**：已实现但实验性（Erasure parity/乐观锁/GC 可选/Dedup 降预留）、未实现（压缩/流式/Reed-Solomon 纠删码/CLI 接线/内容寻址去重）、KDF 档位扩展、units 单位抽象——每项给状态+演进路径
9. 已知限制（CLI 未接线加密卷文件面、config 一次性 secretdata 装配依赖排序）

- [ ] **步骤 2：历史决策归档 `docs/designs/2026-10-01-secret-volume-decisions.md`**

归档方案反复的 WHY（维护者理解「为什么是这样」），按主题：
- 目录保密演进（data/meta 目录 → 随机容器 → 父引用解耦）
- 删除语义（墓碑+GC → 方案 A 即时物理删 + GC 可选）
- KDF 档位化（常量 → Algorithm 档位 → 移除 test 档 → mockkdf 子包）+ loadGate 自适应
- 字节单位（int64 → internal/size → pkg/units/sizex）
- 过度设计收敛（Dedup 降预留、Erasure 标实验性）
- 对抗性审查驱动的收敛记录（5 轮审查 Critical 0 的路径）

- [ ] **步骤 3：包 README**

`pkg/cryptox/shardseal/README.md`：算法包（能力：分块加密/统一格式/blocklet/算法注册表/KDF 档位/mockkdf；格式说明；构建）；`pkg/volume/secretdata/README.md`：卷包（能力：布局/目录解耦/即时删/GC/冗余/寻址；用法；对齐 baidupcs 风格）。两者均注明「未来方向见 docs/secret-volume.md §8」。

- [ ] **步骤 4：设计文档头部加指针**

`docs/designs/2026-10-01-secret-volume.md` 头部加：「功能文档：docs/secret-volume.md；历史决策归档：docs/designs/2026-10-01-secret-volume-decisions.md；包文档：pkg/cryptox/shardseal/README.md、pkg/volume/secretdata/README.md」。

- [ ] **步骤 5：验证 + Commit**

文档无需编译（跑 gofmt 确认无 .go 误改）。`git add docs/ pkg/ && git commit -m "docs(secret): 功能文档 + 历史决策归档 + 包 README——面向使用者与未来方向"`。注意与正在跑的过时注释清理（a4ce5013c48569494）不冲突（不同文件）。

**报告契约：** 写入 `D:\workdir\leon\cocomhub\sproxy-secret-worktree\.superpowers\sdd\2026-10-02-secret-volume-directory-privacy\task-16-report.md`，只回传：状态、提交 hash、产出文件清单、疑虑。
