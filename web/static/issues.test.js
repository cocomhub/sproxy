// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// issues.test.js node --test 单测（「待处理事项」渲染纯函数 + 请求构造）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./issues.js');

test('issueStatusLabel 状态标签与颜色（needs_human 必须最醒目）', () => {
  assert.strictEqual(m.issueStatusLabel('needs_human'), '需人工接入');
  assert.strictEqual(m.issueStatusLabel('open'), '待处理');
  assert.strictEqual(m.issueStatusLabel('resolved'), '已解决');
  assert.notStrictEqual(m.issueStatusColor('needs_human'), m.issueStatusColor('open'));
});

test('issuesStatsHtml 空状态给出明确提示', () => {
  assert.ok(m.issuesStatsHtml({}).includes('暂无待处理事项'));
  assert.ok(m.issuesStatsHtml(null).includes('暂无待处理事项'));
});

test('issuesStatsHtml 展示各状态计数', () => {
  const html = m.issuesStatsHtml({ needs_human: 2, open: 1, resolved: 3 });
  assert.ok(html.includes('需人工接入'));
  assert.ok(html.includes('>2<'));
  assert.ok(html.includes('>1<'));
});

test('issuesListHtml 空列表提示', () => {
  assert.ok(m.issuesListHtml({ items: [] }).includes('未发现待处理事项'));
});

test('issuesListHtml 渲染关键列 + 人工解决按钮 + 详细原因', () => {
  const html = m.issuesListHtml({
    items: [{
      id: 'dir_meta_ambiguous:abc', kind: 'dir_meta_ambiguous', volume: 'baidu',
      path: 'ACME [OypE/abc]', status: 'needs_human', retryable: false, attempts: 0,
      detail: '容器内有 2 个 dir-meta；建议：人工仲裁后经 GC 清理旧 blob',
      last_error: '删除旧目录 meta 失败',
    }],
  });
  assert.ok(html.includes('dir_meta_ambiguous'));
  assert.ok(html.includes('baidu'));
  assert.ok(html.includes('需人工决策'));
  assert.ok(html.includes('人工仲裁'));          // 详细失败原因必须可见
  assert.ok(html.includes('删除旧目录 meta 失败')); // 最近失败也要可见
  assert.ok(html.includes('issues-resolve-btn'));
  assert.ok(html.includes('data-id="dir_meta_ambiguous:abc"'));
});

test('issuesListHtml 可重试项标注重试次数', () => {
  const html = m.issuesListHtml({
    items: [{ id: 'x', kind: 'container_not_indexed', volume: 'v', path: 'p', status: 'open', retryable: true, attempts: 2, detail: 'd' }],
  });
  assert.ok(html.includes('可自动重试'));
  assert.ok(html.includes('已试 2 次'));
});

test('issuesListHtml 转义（防注入）', () => {
  const html = m.issuesListHtml({ items: [{ id: 'a"b', kind: 'k', volume: '<v>', path: 'p', status: 'open', detail: '<script>' }] });
  assert.ok(!html.includes('<script>'));
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(html.includes('&quot;'));
});

test('issuesFiltersHtml 含筛选控件与两个动作按钮', () => {
  const html = m.issuesFiltersHtml({ status: 'needs_human', volume: 'baidu', unresolved: true });
  assert.ok(html.includes('id="issues-status"'));
  assert.ok(html.includes('id="issues-volume"'));
  assert.ok(html.includes('value="baidu"'));
  assert.ok(html.includes('checked'));
  assert.ok(html.includes('id="issues-scan-btn"'));
  assert.ok(html.includes('selected'));
});

test('issuesPanelHtml 组合汇总+筛选+列表', () => {
  const html = m.issuesPanelHtml({ stats: { needs_human: 1 }, items: [] }, {});
  assert.ok(html.includes('需人工接入'));
  assert.ok(html.includes('未发现待处理事项'));
  assert.ok(html.includes('issues-refresh-btn'));
});

test('issuesDetailHtml 含 ID/原因/解决说明', () => {
  const html = m.issuesDetailHtml({ id: 'i1', status: 'resolved', kind: 'k', volume: 'v', path: 'p', detail: 'd', resolution: '人工已处置' });
  assert.ok(html.includes('i1'));
  assert.ok(html.includes('人工已处置'));
  assert.ok(!m.issuesDetailHtml(null).includes('i1'));
});

test('issuesResolveQuery 对 id/note 做 URL 编码（note 含空格中文不得破坏请求）', () => {
  const q = m.issuesResolveQuery('a b', '已确认保留 Version 较大者并经 GC 删除旧 blob');
  assert.ok(q.startsWith('/api/issues/resolve?id='));
  assert.ok(!q.includes(' '));
  assert.ok(q.includes('note='));
  assert.ok(q.includes('%20') || q.includes('+'));
});

test('issuesListQuery 组装筛选参数（空则不带头）', () => {
  assert.strictEqual(m.issuesListQuery({}), '/api/issues');
  assert.strictEqual(m.issuesListQuery({ status: 'needs_human', volume: 'baidu', unresolved: true }),
    '/api/issues?status=needs_human&volume=baidu&unresolved=1');
  assert.strictEqual(m.issuesListQuery({ kind: 'dir_meta_ambiguous' }), '/api/issues?kind=dir_meta_ambiguous');
});
