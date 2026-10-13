# Secretdata（加密卷）CLI 文件面收口设计（2026-10-13）

> 状态：已实现（master）。关联权威文档：`docs/secret-volume.md` §8.2/§9。
> 触发：对 secretdata 卷做 CLI 面实测（真 sproxy + 真 sclient），发现**静默错误**与**功能缺口**。

## 1. 实测发现（修复前）

| # | 现象 | 根因 |
|---|---|---|
| ① | `sclient --volume vault list` **静默返回空** | `files.Service.List` 走**搜索索引**（只遍历本地卷的 `storage.Root`）；外部/加密卷没有 Root ⇒ 未被列举，且**不报错**（用户以为卷是空的） |
| ② | `sclient --volume vault mv a b` 恒失败「源文件 checksum 为空，无法重命名」 | `StatPath` 的**外部卷分支**（`dp.Source != nil`）只回 IsDir/Size/MTime，**不填 checksum**（本地分支才取台账/实时算） |
| ③ | 同上，绕过 checksum 门禁后服务端 404「源文件不存在」 | `RenameFile` 的 `resolveRenameHome` 只覆盖本地卷 ⇒ 外部卷文件无法重命名 |
| ④ | 即便定位成功，`SecretdataFS.Rename` 对**文件改 basename** fail-closed | 属设计：basename 锚定在加密 meta 内（`Meta.Original.Name`），物理原地改名不成立 |

（`upload`/`download`/`Range 播放`/云转存/`delete` 此前已可用。）

## 2. 处置

1. **列举**：`files.List` 在**显式 `?volume=`** 时优先经新增接缝 `ExternalVolumeOps.ExternalList`
   列举外部卷（经同一可信链 `Guard(Wrap(be.FS()))` + `ResolveUserLocation` 键翻译）。
   非外部卷支持返回 `supported=false` → 落回既有索引语义（**零回归**）。
   未指定卷时不触达外部卷（避免每次列表都打远端 API）——**记为已知限制**并文档化。
2. **checksum**：新增可选能力 `files.ChecksumSource`（`DownloadSource` 的可选扩展）——
   `StatPath` 外部卷分支对实现该能力的读取源填 `Checksum`（明文整文件 SHA-256，来自
   `meta.Provider`：secretdata 的 `Original.SHA256` / trusted sidecar 记录值）；未实现 →
   保持旧行为（空，不伪造）。
3. **改名**：新增接缝 `ExternalVolumeOps.ExternalRename`（同卷语义；目标已存在 409、
   checksum 不一致 409、封印 409）；本地优先、未命中转外部（与删除路径同款，避免为本地
   重命名触达远端）。
4. **改名 = 只重写 meta（2026-10-13 修正）**：`SecretdataFS.Rename` 对「文件改 basename」改为
   **原生支持**——文件名只存在于加密 meta 内（`Original.Name`；meta blob 名派生自密文内容 ⇒ 改名
   换名重写 **meta**），**分块内容零改动、不重加密**（分块名 = 密文内容哈希，与逻辑名无关）；
   **先写新 meta 成功、再删旧 meta** ⇒ 崩溃窗口最多两份 meta（不丢数据）；耗时 **O(meta)** 而非
   O(内容)。契约哨兵 `syncpkg.ErrUnsupported` + 装配层「读源 → 写目标 → 删源」兜底**保留**给其它
   无法原地改名的后端。

## 3. 测试与证据

- e2e（真二进制）：`TestE2E_CLI_Secretdata_CliFileOps`（upload → list/list --json → meta 带
  checksum → download 逐字节 → mv → 旧名不可见/新名一致/磁盘仍加密容器 → delete 即时删净）；
  `TestE2E_CLI_Secretdata_DeclaredBeforeSecrets`（**卷声明顺序无关**——修正文档旧声称）。
- 单测：`pkg/files`（`ChecksumSource` 生效 / 未实现时保持空）、`pkg/server`
  （`copyThenDeleteExternal`：内容搬移 + 删源；**写失败必须保留源**）、`pkg/volume/secretdata`
  （改名返回契约哨兵）。
- **变异 6 命中**：StatPath 不取 checksum／拷贝后不删源／先删后写／secretdata 不返回哨兵／
  去掉外部卷列举分支（e2e 红）／去掉 ErrUnsupported 回退（e2e 红）。

## 3.1 卷 URI 寻址（用户要求 2026-10-13）

位置参数写 `<卷名>:/<路径>`（`<卷名>:` = 卷根）⇒ 等价 `--volume <卷名>` + 纯路径。**纯客户端归一**
（只改写本地参数与 flag；线上仍是 `?volume=` 查询参数，协议/服务端零改动）：新增
`cmd/sclient/volume_uri.go`（`splitVolumeURI`/`applyVolumeURIs`/`applyMvVolumeURIs`），接入
`list`/`download`/`meta`/`delete`/`du`/`mv`（`upload` 的位置参数是本地文件、`search` 是查询串，
故不接入）。歧义按语法消除：要求 `:/`（冒号后单个斜杠）⇒ `https://…` 与 `C:\…` 不误判；
位置参数与显式 `--volume` 卷名不一致 → **报错**（不静默取其一）。`mv` 两端带不同卷名 → 自动
按跨卷迁移（`--to-volume`）。

测试：`cmd/sclient/volume_uri_test.go`（解析表 + flag 归一 + 冲突 + mv 跨卷）；e2e
`TestE2E_CLI_Secretdata_CliFileOps` 全程以 **URI 形式**驱动加密卷操作。文档：`docs/cli.md`
「卷 URI 寻址」小节。

## 4. 仍开（记档）

- 未指定 `--volume` 的列表仍只含本地卷（聚合外部卷需要「列表也走远端」的成本口径决策）。
- ~~文件改名 = O(内容) 重写~~ **已收口（2026-10-13）**：改为「只重写 meta」（O(meta)，内容零
  改动），见 §2 第 4 条。
- secrets 卷外部 target / secretdata 外部 target（外部卷作 target）仍为预留片（fail-closed）。

## 3.2 卷感知工作目录 + mkdir 接线（2026-10-13 追加，用户口径）

**用户口径**：① 工作目录支持卷 URI（`cd A:/path`）；② 所有操作（list/pwd/相对路径）默认只作用于
**当前工作目录所在卷**；③ 需操作别处时用 URI；④ 以 `/` 开头 = **本地卷**根，其它卷必须写 `A:/`。

**落地**：
- `state.State` 增 `CurrentVolume`（**独立缓存文件** `current_volume`，旧缓存缺该文件 = 本地卷，
  零兼容负担）+ `Location()`（`A:/dir` 或 `/dir`）；`rootPreRunE` 载入。
- `cd`：`<卷名>:/<路径>` 切卷（路径 = 该卷根绝对）；`/…` 显式回本地卷并从根起算；相对路径/`..` 保持卷。
  `pwd` 打印 `<卷名>:/<目录>`。
- 命令侧统一 `applyVolumeContext(cmd, st, args)`：**位置参数卷 URI > `--volume` > 工作目录所在卷 >
  auto**（冲突报错）；`applyMvVolumeContext` 对 `mv` 两端做同样归一（目标不同卷 ⇒ `--to-volume`）。
  `upload` 只应用工作目录卷（其位置参数是本地文件）；`copy/move`（`volume` 子命令）同样接入。
- **URI 路径语义**：`splitVolumeURI` 返回**保留前导 `/`** 的绝对路径 ⇒ 交给既有 `ResolveRemotePath`
  时绕过 `cd` 前缀（否则 `list vault:/` 会被当成「当前目录相对」，实测踩过）。
- **顺带修掉的真 bug（实测暴露）**：
  1. `pkg/client` 的 `Mkdir`/`Rmdir` 手拼 URL **漏带 `volume` 查询参数** ⇒ `--volume`/URI 静默失效
     （mkdir 落到本地卷，加密卷上「目录已创建」却找不到）；已补 + 单测钉住。
  2. `files.Service.MakeDir` 无卷参数 ⇒ 显式卷被忽略（恒落 primary 视图卷）；新增 `volName` +
     `ExternalVolumeOps.ExternalMkdir` 接缝（外部卷目录建在**该卷**）；显式本地卷也按 `volumeTenant` 定位。
  3. `rmdir` 显式外部卷 ⇒ **fail-closed 501**（此前会静默删本地同名目录——危险方向）。
  4. `list --json` 空结果此前输出**空串**（JSONFormatter.Println 被忽略）⇒ 非法 JSON；改为输出
     `{"files":[],"total":0}`。

**测试**：`cmd/sclient`（cd 卷 URI/持久化/`/` 回本地卷/相对保持卷、`pwd`、URI 解析与优先级、
`ApplyMv*` 跨卷）、`pkg/client`（mkdir/rmdir 携带卷参数）、e2e
`TestE2E_CLI_VolumeCwd_DefaultsToCurrentVolume`（cd 到加密卷 → 相对 upload/list/mkdir/download 全在
该卷 → `cd /` 回本地卷 → rmdir 加密卷 fail-closed）。**变异命中**：URI 去掉 URL 排除；URI 去掉冲突报错；
mkdir 客户端去掉卷参数；同容器改名早退去掉。

**仍开**：跨卷移动/复制对**外部/加密卷**未接线（服务端 `400 无效的文件路径`；需源读 + 目标写 + 源删，
经统一写内核与外部卷接缝）；加密卷 `rmdir` 未接线（`syncpkg.FS` 缺目录级递归删）。
