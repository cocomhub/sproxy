/**
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// 生产稳定性对抗审查：四个区域并行只读审查（fresh context）。
const REPO = 'D:/workdir/leon/cocomhub/sproxy';

const COMMON = `
仓库：${REPO}（Go，分支 master，master 已合入 PR #840）。
任务性质：**只读审查**，禁止修改任何文件，禁止 git 写操作。

审查目标：找出**会导致生产不稳定/数据损坏/静默错误**的缺陷（P0 = 数据丢失/安全/崩溃/静默错误；P1 = 功能缺陷、恢复不可用、资源泄漏、无限等待）。
**不要报告**：风格、命名、注释、文档措辞、测试覆盖率不足这类非生产阻塞项；也不要重复已被文档记载并标记"已修"的项。
**必须**：每条缺陷给 文件:行号 + 触发条件 + 后果 + 为什么现有测试挡不住（如果挡住了就别报）。
**必须**：对每条结论做一次自我证伪（读相关调用方/测试确认不是误读），并在输出里写"证伪结果"。
**置信度**：只报你能用代码证据支撑的；推测请显式标 [推测]。

输出格式（简洁，中文）：
## 结论
（该区域当前是否达到生产稳定可用；一句话）
## 发现
### [P0|P1] 标题
- 位置：file:line
- 触发：…
- 后果：…
- 现有测试为何挡不住：…
- 建议修法（最小改动）：…
- 证伪结果：…
## 已核实无问题的高风险点
（列出你重点检查过且确认正确的 3-6 个点，一句话each）
`;

const lanes = [
  {
    key: 'secretdata',
    task: `${COMMON}
## 你负责的区域：secretdata 卷（加密分卷卷，pkg/volume/secretdata/**）

重点（按生产风险排序）：
1. 写入原子性/回滚：覆盖写、改名、删除在**中途失败**（磁盘满、远端 5xx/超时、context 取消）时是否留下半态、meta 与 blob 不一致、旧版本不可读。
2. 并发：同名并发写、并发 rename、GC/回收与读并发（是否有 map 竞态、删除正在读的 blob）。
3. 读路径：RangeReader/分块解密是否可能返回**错误数据但报成功**（长度不足、块校验跳过、越界）；临时文件峰值与清理。
4. 外部卷（inner 是 baidupcs/s3 之类远端）时的错误传播：远端失败是否被当成功、重试是否有界（可能无限重试/永久挂起）。
5. 容量/物理足迹记账：失败路径是否漏回滚（账本漂移是否会无限累积）。
入口建议：pkg/volume/secretdata/secretdata.go（WriteFile/deliverLocalPath/rename/delete/read）、features.go、read_seeker.go、erasure.go、replica_fs.go、gc*/tombstone。`,
  },
  {
    key: 'baidupcs',
    task: `${COMMON}
## 你负责的区域：百度网盘外部卷（pkg/volume/ext/baidupcs/**）

重点：
1. 上传/下载的**重试语义**：是否可能无限重试、是否在重试中重复扣费/重复创建、错误是否被吞（把失败当成功）。
2. 秒传（rapidupload）与 md5 不可靠（MD5Unreliable）时的完整性口径：是否可能把**错误内容**当成功写入/读出。
3. 分片上传会话恢复：中断后 resume 的幂等性（同 key 重复上传、session 过期、slice md5 校验）。
4. staging 本地副本：峰值空间是否可能 2×（Put 与 PutFromStagedPath 的差异）、staging 残留是否会被回收（会否长期泄漏磁盘）。
5. 并发：同一文件并发写/读、token 过期重登（并发刷新是否互相覆盖）、RateLimiter/错误码映射（风控码被当业务错误无限重试）。
入口建议：storage.go（Put/PutFromStagedPath/prepareStagedForAdapter/putStaged）、adapter.go、syncfs.go、resume_upload.go、api.go、binary_meta.go、quota_test.go。`,
  },
  {
    key: 'transfer',
    task: `${COMMON}
## 你负责的区域：转存（云端下载 → 卷转存，pkg/cloud/transfer.go、pkg/cloud/manager_task.go、pkg/server/cloud_archive*.go、pkg/server/cloud_download_handler.go）

重点：
1. 转存的**失败/回滚**：写入目标卷失败、配额不足、staging 满、ctx 取消时，是否留下半文件/孤儿 staging/泄漏预留（quota 未 release）。
2. 幂等与重复执行：重启恢复后重复转存是否会覆盖已存在对象、是否会双倍计入容量。
3. 目标卷解析（TransferFSFor / volumeFor / transferRelPath）：是否存在**路径逃逸**（../）、卷名混淆、外部卷与本地卷口径混用导致的越权/越界写。
4. 下载→转存窗口：产物校验（size/checksum）失败时的处理；`DisableDoubleDownloadVerify` 的规格与实现是否一致（文档缺口），不一致会不会导致**不校验就提交**。
5. 并发：同任务并发恢复（两个 worker 同时处理同一 task）、同对象并发转存（互覆盖/账本双记）。
入口建议：pkg/cloud/transfer.go、manager_task.go、manager_persist.go、pkg/server/cloud_archive.go、cloud_archive_quota*.go、handlers.go 里 cloudArchiveToVolume。`,
  },
  {
    key: 'audit-recovery',
    task: `${COMMON}
## 你负责的区域：审计 WAL 与恢复（pkg/audit/**、pkg/recovery/**、pkg/server/recovery_wiring.go、pkg/cloud 的恢复入口）

重点：
1. WAL 崩溃矩阵：段尾部**半写记录**、部分写入的 header、fsync 前后崩溃 ⇒ 恢复时是否会**丢已提交记录**或**重复应用**（幂等）；序号（seq）跨重启是否单调（曾修过 initSeq，确认没有别的路径绕过）。
2. MarkDone/提交记录语义：落盘失败时是否真的回滚 + 上抛（而不是 best-effort 吞掉 ⇒ 任务被判未完成而重复执行）。
3. 冷热归档：归档失败/部分成功时的可恢复性（会不会**删掉还没归档的段**）；`shiftArchives`/`PurgeCold` 的失败路径。
4. 读面：`ReadTail`/`ReadTailCold` 在有损坏段/半写段时是否返回**截断但成功**（静默丢数据）；limit/offset 语义。
5. 恢复入口的**有界性**：坏文件/超大目录/不可读段是否会导致启动挂起或内存爆炸（io.ReadAll 无上限）。
入口建议：pkg/audit/{wal,journal,store,archive,cold,service,core}.go、pkg/recovery/recovery.go、pkg/server/recovery_wiring.go、pkg/cloud/manager_persist.go 的 recoverTasksFromDir。`,
  },
];

const results = await runs.all(lanes.map((l) => ({
  key: l.key,
  agent: 'reviewer',
  task: l.task,
  acceptance: false,
})));
for (let i = 0; i < lanes.length; i++) {
  emit('===== ' + lanes[i].key + ' =====');
  emit(String(results[i] && results[i].output ? results[i].output : '(no output)'));
}
return 'done';
