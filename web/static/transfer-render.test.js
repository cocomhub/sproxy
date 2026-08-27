/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * transfer-render.test.js —— app-render.js 传输渲染纯函数单元测试。
 *
 * 运行：node --test web/static/transfer-render.test.js（已并入 make web-test）。
 * 覆盖：传输状态频道条定义 + filterTransferItems 全频道 + buildTransferRowHtml
 *      （data-item-id / 进度 / 状态徽章 / 操作按钮组）+ buildTransferListHtml
 *      （过滤 + 已完成折叠分组 + 空文案）。纯函数测试，不碰 DOM。
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const r = require(path.join(__dirname, 'app-render.js'));

// 覆盖五类 kind + 全部状态（upload 含 hashing/uploading/paused/failed/cancelled/completed；
// download 含 archive 下载；cloud_task/cloud_group 三类状态）。
function sampleItems() {
  return [
    { id: 'u-hashing', kind: 'upload', filename: 'a.bin', status: 'hashing', meta: { totalChunks: 4 }, loaded: 0, total: 100 },
    { id: 'u-uploading', kind: 'upload', filename: 'b.bin', status: 'uploading', meta: { totalChunks: 4, chunksBitmap: [1, 1, 0, 0] }, loaded: 50, total: 100 },
    { id: 'u-paused', kind: 'upload', filename: 'c.bin', status: 'paused', meta: { totalChunks: 4, chunksBitmap: [1, 0, 1, 0] }, loaded: 40, total: 100 },
    { id: 'u-failed', kind: 'upload', filename: 'd.bin', status: 'failed' },
    { id: 'u-cancelled', kind: 'upload', filename: 'e.bin', status: 'cancelled' },
    { id: 'u-completed', kind: 'upload', filename: 'f.bin', status: 'completed', meta: { dirname: 'dir1' } },
    { id: 'd-downloading', kind: 'download', filename: 'g.tar.gz', status: 'downloading', meta: { archive: true, totalChunks: 8, chunksBitmap: [1, 0, 0, 0, 0, 0, 0, 0] }, loaded: 10, total: 100 },
    { id: 'd-paused', kind: 'download', filename: 'h.bin', status: 'paused' },
    { id: 'd-failed', kind: 'download', filename: 'i.bin', status: 'failed' },
    { id: 'd-cancelled', kind: 'download', filename: 'j.bin', status: 'cancelled' },
    { id: 'd-completed', kind: 'download', filename: 'k.bin', status: 'completed', meta: {} },
    { id: 'ct-running', kind: 'cloud_task', filename: 'm.bin', status: 'downloading', loaded: 10, total: 20 },
    { id: 'ct-failed', kind: 'cloud_task', filename: 'n.bin', status: 'failed' },
    { id: 'ct-completed', kind: 'cloud_task', filename: 'o.bin', status: 'completed', meta: {} },
    { id: 'cg-running', kind: 'cloud_group', name: 'p', status: 'downloading' },
    { id: 'cg-failed', kind: 'cloud_group', name: 'q', status: 'failed' },
    { id: 'cg-completed', kind: 'cloud_group', name: 'r', status: 'completed' },
  ];
}

// ---- 频道定义 ----

test('TRANSFER_CHANNELS 频道顺序与 label 精确取值', () => {
  assert.deepStrictEqual(r.TRANSFER_CHANNELS.map((c) => c.id),
    ['all', 'uploading', 'downloading', 'cloud_tasks', 'cloud_groups', 'completed']);
  const labels = {};
  for (const c of r.TRANSFER_CHANNELS) labels[c.id] = c.label;
  assert.deepStrictEqual(labels, {
    all: '全部', uploading: '上传中', downloading: '下载中',
    cloud_tasks: '云任务', cloud_groups: '云组', completed: '已完成',
  });
});

// ---- filterTransferItems ----

test('filterTransferItems: all 频道返回全部（顺序保留）；缺省/空频道回落 all', () => {
  const all = r.filterTransferItems(sampleItems(), 'all');
  assert.deepStrictEqual(all.map((i) => i.id), sampleItems().map((i) => i.id));
  assert.strictEqual(r.filterTransferItems(sampleItems(), null).length, sampleItems().length);
  assert.strictEqual(r.filterTransferItems(sampleItems(), undefined).length, sampleItems().length);
});

test('filterTransferItems: uploading 频道只读 upload 类（hashing/uploading/paused/failed/cancelled），不含 completed upload', () => {
  const out = r.filterTransferItems(sampleItems(), 'uploading');
  assert.deepStrictEqual(out.map((i) => i.id), ['u-hashing', 'u-uploading', 'u-paused', 'u-failed', 'u-cancelled']);
  assert.ok(!out.some((i) => i.id === 'u-completed'));
  assert.ok(out.every((i) => i.kind === 'upload'));
});

test('filterTransferItems: downloading 频道含 archive 下载，只读 download 类进行中/失败/取消', () => {
  const out = r.filterTransferItems(sampleItems(), 'downloading');
  assert.deepStrictEqual(out.map((i) => i.id), ['d-downloading', 'd-paused', 'd-failed', 'd-cancelled']);
  assert.ok(!out.some((i) => i.id === 'd-completed'));
  assert.ok(out.every((i) => i.kind === 'download'));
});

test('filterTransferItems: cloud_tasks / cloud_groups 按 kind 全量透传（含 completed）', () => {
  assert.deepStrictEqual(r.filterTransferItems(sampleItems(), 'cloud_tasks').map((i) => i.id),
    ['ct-running', 'ct-failed', 'ct-completed']);
  assert.deepStrictEqual(r.filterTransferItems(sampleItems(), 'cloud_groups').map((i) => i.id),
    ['cg-running', 'cg-failed', 'cg-completed']);
});

test('filterTransferItems: completed 含各 kind 的 completed（upload/download/cloud_task/cloud_group）', () => {
  const out = r.filterTransferItems(sampleItems(), 'completed');
  assert.deepStrictEqual(out.map((i) => i.id), ['u-completed', 'd-completed', 'ct-completed', 'cg-completed']);
});

test('filterTransferItems: 未知频道 fail-closed 返回 []', () => {
  assert.deepStrictEqual(r.filterTransferItems(sampleItems(), 'bogus'), []);
  assert.deepStrictEqual(r.filterTransferItems(sampleItems(), ''), []);
});

// ---- buildTransferRowHtml ----

test('buildTransferRowHtml 上传行：data-item-id + 进度 + 状态徽章 + 已缓存 X/Y + 暂停/取消', () => {
  const html = r.buildTransferRowHtml({
    id: 'x<1', kind: 'upload', filename: 'a<b.bin', status: 'uploading', loaded: 50, total: 100,
    meta: { totalChunks: 4, chunksBitmap: [1, 1, 0, 0] },
  });
  assert.ok(html.includes('data-item-id="x&lt;1"'), 'id 应转义置于行根');
  assert.ok(html.includes('a&lt;b.bin'), 'filename 转义');
  assert.ok(html.includes('50%'), '进度百分比');
  assert.ok(html.includes('已缓存 2/4 块'), 'chunksBitmap 置位计数');
  assert.ok(html.includes('暂停') && html.includes('取消'), '进行中操作按钮');
  assert.ok(!html.includes('<script'), '无 XSS');
});

test('buildTransferRowHtml 状态徽章用 statusText（下载行含重新下载、暂停/取消按钮）', () => {
  const html = r.buildTransferRowHtml({ id: 'd1', kind: 'download', filename: 'g.tar.gz', status: 'downloading', loaded: 10, total: 100, meta: { totalChunks: 8, chunksBitmap: [1, 0, 0, 0, 0, 0, 0, 0] } });
  assert.ok(html.includes('⬇ 下载中'), '状态徽章走 statusText');
  assert.ok(html.includes('已缓存 1/8 块'));
  assert.ok(html.includes('暂停') && html.includes('取消'));
});

test('buildTransferRowHtml completed 上传项：打开存储目录 + 删除记录', () => {
  const html = r.buildTransferRowHtml({ id: 'u-c', kind: 'upload', filename: 'f.bin', status: 'completed', meta: { dirname: 'dir1' } });
  assert.ok(html.includes('✅ 已完成'));
  assert.ok(html.includes('打开存储目录'));
  assert.ok(html.includes('删除记录'));
  assert.ok(!html.includes('暂停'));
});

test('buildTransferRowHtml completed 下载项：重新下载 + 删除记录（无存储目录）', () => {
  const html = r.buildTransferRowHtml({ id: 'd-c', kind: 'download', filename: 'k.bin', status: 'completed' });
  assert.ok(html.includes('重新下载'));
  assert.ok(html.includes('删除记录'));
  assert.ok(!html.includes('打开存储目录'));
});

test('buildTransferRowHtml failed/ paused 项：恢复/取消按钮', () => {
  const fails = r.buildTransferRowHtml({ id: 'x', kind: 'upload', filename: 'd.bin', status: 'failed' });
  assert.ok(fails.includes('恢复') && fails.includes('删除'));
  assert.ok(!fails.includes('暂停'));
  const paused = r.buildTransferRowHtml({ id: 'y', kind: 'upload', filename: 'c.bin', status: 'paused' });
  assert.ok(paused.includes('恢复') && paused.includes('取消'));
});

// ---- buildTransferListHtml ----

test('buildTransferListHtml 空列表或频道无匹配 → 暂无传输记录', () => {
  assert.ok(r.buildTransferListHtml([], 'all').includes('暂无传输记录'));
  assert.ok(r.buildTransferListHtml(sampleItems(), 'bogus').includes('暂无传输记录'));
});

test('buildTransferListHtml all：进行中行 + 已完成按 kind 折叠分组（summary+detail 展开机制）', () => {
  const html = r.buildTransferListHtml(sampleItems(), 'all');
  // 进行中项直接渲染为行
  assert.ok(html.includes('data-item-id="u-uploading"'));
  assert.ok(html.includes('data-item-id="d-downloading"'));
  // 已完成折叠：按 kind 分组，detail id 为 group-detail-<kind>，默认隐藏
  assert.ok(html.includes('id="group-detail-upload"'));
  assert.ok(html.includes('id="group-detail-download"'));
  assert.ok(html.includes('id="group-detail-cloud_task"'));
  assert.ok(html.includes('id="group-detail-cloud_group"'));
  // 折叠分组行 summary 显示分组计数与 kind 标签
  assert.ok(html.includes('已完成上传 (1)'));
  assert.ok(html.includes('已完成下载 (1)'));
  // 展开机制：details 未带 open → 默认折叠；含可点击 summary 元素
  assert.ok(!/<details[^>]*open/.test(html));
  assert.ok(html.includes('<summary'));
  // completed 项行内容在 detail 分组内（含 data-item-id + 打开存储目录）
  assert.ok(html.includes('data-item-id="u-completed"'));
  assert.ok(html.indexOf('u-completed') > html.indexOf('group-detail-upload'));
});

test('buildTransferListHtml 频道过滤联动：downloading 频道只含 download 行，无其它 kind 泄漏', () => {
  // 注：downloading 频道 completed 项被 filterTransferItems 排除（spec-defined），
  // 故该频道不出现折叠组——折叠组只在 all/completed 频道出现。
  const html = r.buildTransferListHtml(sampleItems(), 'downloading');
  assert.ok(html.includes('data-item-id="d-downloading"'));
  assert.ok(!html.includes('u-uploading'));
  assert.ok(!html.includes('group-detail-upload'));
  assert.ok(!html.includes('group-detail-cloud_task'));
  assert.ok(!html.includes('group-detail-cloud_group'));
});

test('buildTransferListHtml completed 全折叠：仅分组 summary，detail 默认隐藏但含行', () => {
  const html = r.buildTransferListHtml(sampleItems(), 'completed');
  assert.ok(html.includes('已完成上传 (1)'));
  assert.ok(html.indexOf('u-completed') > html.indexOf('group-detail-upload'));
  // 折叠成立：无 open 属性（默认折叠）
  assert.ok(!/<details[^>]*open/.test(html));
});
