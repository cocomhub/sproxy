/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * app-render.test.js —— app-render.js 纯渲染模块全场景单元测试。
 *
 * 运行：node --test web/static/app-render.test.js（已并入 make web-test）。
 * 覆盖：基础工具 / 列表 / loadMore / hub / config / stats / 云端 / 版本 全纯函数。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const r = require(path.join(__dirname, 'app-render.js'));

// ---- 基础工具 ----
test('formatSize 各档位', () => {
  assert.strictEqual(r.formatSize(0), '0 B');
  assert.strictEqual(r.formatSize(1023), '1023 B');
  assert.strictEqual(r.formatSize(1024), '1.0 KB');
  assert.strictEqual(r.formatSize(1048576), '1.0 MB');
  assert.strictEqual(r.formatSize(1073741824), '1.00 GB');
  assert.strictEqual(r.formatSize(null), '-');
  assert.strictEqual(r.formatSize(undefined), '-');
});

test('escHtml 转义', () => {
  assert.strictEqual(r.escHtml('a&b<c>"d'), 'a&amp;b&lt;c&gt;&quot;d');
  assert.strictEqual(r.escHtml(null), '');
  assert.strictEqual(r.escHtml(undefined), '');
});

test('uploadProgressText 可测入口：进度文案与 upload.js progressText 语义一致', () => {
  // 分块阶段（totalChunks>1 且带 chunkIndex → 分块 i/N）
  assert.deepStrictEqual(r.uploadProgressText('上传中…', 12, 100, 4, 1), { pct: 12, text: '上传中… 12%（12 B/100 B，分块 2/4）' });
  // 计算阶段（无分块）
  assert.deepStrictEqual(r.uploadProgressText('计算 SHA-256…', 5, 10), { pct: 50, text: '计算 SHA-256… 50%（5 B/10 B）' });
  // 边界：total=0 → pct 0；chunkIndex 缺省 → 分块 1/N
  assert.deepStrictEqual(r.uploadProgressText('上传中…', 0, 0, 3), { pct: 0, text: '上传中… 0%（0 B/0 B，分块 1/3）' });
  assert.deepStrictEqual(r.uploadProgressText('下载中…', 7, 8, 2, 0), { pct: 88, text: '下载中… 88%（7 B/8 B，分块 1/2）' });
});

test('getChecksumPrefix + bytesToHex', () => {
  assert.strictEqual(r.getChecksumPrefix(''), '-');
  assert.strictEqual(r.getChecksumPrefix('abc'), 'abc…');
  assert.strictEqual(r.getChecksumPrefix('a'.repeat(20)), 'a'.repeat(16) + '…');
  assert.strictEqual(r.bytesToHex(new Uint8Array([0, 15, 255])), '000fff');
  assert.strictEqual(r.bytesToHex(null), '');
});

test('normalizeList：数组 / {files} / 其它 / 空', () => {
  assert.deepStrictEqual(r.normalizeList([1, 2]), [1, 2]);
  assert.deepStrictEqual(r.normalizeList({ files: [3] }), [3]);
  assert.deepStrictEqual(r.normalizeList({ groups: [4] }), []);
  assert.deepStrictEqual(r.normalizeList({ groups: [4] }, 'groups'), [4]);
  assert.deepStrictEqual(r.normalizeList(null), []);
  assert.deepStrictEqual(r.normalizeList(undefined), []);
  assert.deepStrictEqual(r.normalizeList({}), []);
});

test('zipNames：等长 / 短 filenames / 空', () => {
  assert.deepStrictEqual(r.zipNames(['a', 'b'], ['x', 'y']), [{ url: 'a', filename: 'x' }, { url: 'b', filename: 'y' }]);
  assert.deepStrictEqual(r.zipNames(['a', 'b'], ['x']), [{ url: 'a', filename: 'x' }, { url: 'b', filename: '' }]);
  assert.deepStrictEqual(r.zipNames([], []), []);
  assert.deepStrictEqual(r.zipNames(null, null), []);
});

test('parseCloudLines：Tab 拆分 / 空行过滤 / 空白 / 非字符串', () => {
  assert.deepStrictEqual(r.parseCloudLines('a\tb\n\n c \t preset  '), [{ url: 'a', preset: 'b' }, { url: 'c', preset: 'preset' }]);
  assert.deepStrictEqual(r.parseCloudLines('   '), []);
  assert.deepStrictEqual(r.parseCloudLines(''), []);
  assert.deepStrictEqual(r.parseCloudLines(null), []);
  assert.deepStrictEqual(r.parseCloudLines(42), []);
});

test('previewKind 归类', () => {
  assert.strictEqual(r.previewKind('a.jpg'), 'image');
  assert.strictEqual(r.previewKind('dir/x.PNG'), 'image');
  assert.strictEqual(r.previewKind('a.txt'), 'text');
  assert.strictEqual(r.previewKind('a.go'), 'text');
  assert.strictEqual(r.previewKind('a.bin'), 'download');
  assert.strictEqual(r.previewKind('noext'), 'download');
  assert.strictEqual(r.previewKind(''), 'download');
  assert.strictEqual(r.previewKind(null), 'download');
});

// ---- 文件列表排版（HTML 片段断言 + XSS 转义） ----
test('buildFileRowHtml 文件行含转义 + checksum 前缀', () => {
  const html = r.buildFileRowHtml({ name: 'a<b.txt', size: 2048, checksum: 'c'.repeat(20), is_dir: false }, 'a<b.txt');
  assert.ok(html.includes('a&lt;b.txt'));
  assert.ok(html.includes('2.0 KB'));
  assert.ok(html.includes('data-checksum="' + 'c'.repeat(20) + '"'));
  assert.ok(html.includes('cccccccccccccccc…'));
  assert.ok(!html.includes('<script'));
});

test('buildFileRowHtml 目录行', () => {
  const html = r.buildFileRowHtml({ name: 'd', is_dir: true }, 'd');
  assert.ok(html.includes('d/'));
  assert.ok(html.includes('dir-enter-btn'));
});

test('buildFileTableHtml 空 / 多行', () => {
  assert.ok(r.buildFileTableHtml([], 'x').includes('</tbody></table>'));
  const html = r.buildFileTableHtml([{ name: 'f1', size: 10, is_dir: false }, { name: 'd2', is_dir: true }], 'sub');
  assert.ok(html.indexOf('f1') < html.indexOf('d2'));
  assert.ok(html.includes('data-subdir="sub/d2"'));
});

test('buildLoadMoreHtml 参数化 + buildAllLoadedHtml', () => {
  assert.strictEqual(r.buildLoadMoreHtml(5, 3, false), '');
  const m = r.buildLoadMoreHtml(5, 3, true);
  assert.ok(m.includes('加载更多 (2)'));
  assert.ok(m.includes('load-more-container'));
  assert.ok(r.buildAllLoadedHtml(5).includes('已加载全部 5 个文件'));
});

// ---- hub / config / stats ----
test('hubTableHtml 空节点 + 单节点', () => {
  assert.ok(r.hubTableHtml([], null).includes('暂无已连接节点'));
  const html = r.hubTableHtml([{ id: 'n1', addr: '1.2.3.4', connected: '2026-01-01T00:00:00Z' }], { nodes_connected: 1 });
  assert.ok(html.includes('已连接节点: <strong>1</strong>'));
  assert.ok(html.includes('n1'));
  assert.ok(html.includes('hub-remove-btn'));
  assert.ok(!html.includes('<b>'), 'node id 应转义');
});

test('configTableHtml 全字段 + 编辑面板', () => {
  const html = r.configTableHtml({ log_level: 'info', log_format: 'json', access_keys_set: true, rate_limit_requests: 10, rate_limit_window: '1s', max_storage_bytes: 0, chunk_size: 4194304, upload_session_ttl: '24h', versioning_enabled: false, cloud_max_concurrent: 3, addr: ':1', storage_root: '/u', tls_enabled: false, hub_enabled: false });
  assert.ok(html.includes('运行时配置'));
  assert.ok(html.includes('cfg-log-level'));
  assert.ok(html.includes('cfg-update-log-level'));
  assert.ok(html.includes('4.0 MB'));
  assert.ok(!html.includes('4194304'));
});

test('statsTableHtml 各统计', () => {
  const html = r.statsTableHtml({ storage_root: '/d', total_files: 3, total_size: 1024 }, { total: 10, '2xx': 8, '4xx': 1, '5xx': 1 }, { active_connections: 2, files_uploaded: 3, bytes_uploaded: 1024, files_downloaded: 1, bytes_downloaded: 512, files_deleted: 1 });
  assert.ok(html.includes('/d'));
  assert.ok(html.includes('>3<'));
  assert.ok(html.includes('1.0 KB'));
});

// ---- 云端 / 版本 ----
test('statusText 全状态 + 未知', () => {
  assert.strictEqual(r.statusText('pending'), '⏳ 等待中');
  assert.strictEqual(r.statusText('downloading'), '⬇ 下载中');
  assert.strictEqual(r.statusText('completed'), '✅ 已完成');
  assert.strictEqual(r.statusText('failed'), '❌ 失败');
  assert.strictEqual(r.statusText('cancelled'), '🚫 已取消');
  assert.strictEqual(r.statusText('other'), 'other');
});

test('buildProgressBar 封顶 100% + 0 与 5%', () => {
  const h = r.buildProgressBar(150, 100);
  assert.ok(h.includes('width:100%'));
  assert.ok(r.buildProgressBar(0, 0).includes('width:0%'));
  assert.ok(r.buildProgressBar(10, 200).includes('width:5%'));
});

test('cloudTaskActions 各状态', () => {
  assert.ok(r.cloudTaskActions('t1', 'f', 'completed', '').includes('cloud-download-btn'));
  assert.ok(r.cloudTaskActions('t1', 'f', 'failed', '').includes('cloud-resume-btn'));
  assert.ok(r.cloudTaskActions('t1', 'f', 'pending', '').includes('cloud-cancel-btn'));
  assert.ok(!r.cloudTaskActions('t1', 'f', 'completed', '').includes('cloud-cancel-btn'));
});

test('buildCloudTaskTableHtml / buildCloudGroupTableHtml / buildVersionTableHtml', () => {
  const t = r.buildCloudTaskTableHtml([{ id: 't', filename: 'a.bin', status: 'completed', total_size: 100, checksum: 'c' }]);
  assert.ok(t.includes('a.bin'));
  assert.ok(t.includes('✅ 已完成'));
  const g = r.buildCloudGroupTableHtml([{ id: 'g1', name: 'grp', status: 'completed', completed: 1, total_tasks: 2 }]);
  assert.ok(g.includes('grp'));
  assert.ok(g.includes('1/2'));
  // XSS：id 走 escHtml（修复点）
  const gx = r.buildCloudGroupTableHtml([{ id: '<b>x</b>', name: 'n', status: 'x', completed: 0, total_tasks: 0 }]);
  assert.ok(!gx.includes('<b>x</b>'));
  const v = r.buildVersionTableHtml([{ version_id: 1, created_at: 't', size: 5 }], 'f.txt');
  assert.ok(v.includes('共 1 个版本'));
  assert.ok(v.includes('version-restore-btn'));
  assert.ok(v.includes('5 B'));
});

test('buildVersionTableHtml 空数组', () => {
  assert.ok(r.buildVersionTableHtml([], 'f.txt').includes('共 0 个版本'));
});