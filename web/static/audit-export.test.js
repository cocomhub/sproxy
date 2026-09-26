// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-export.test.js node --test 单测（B5 审计导出 + B6 通知测试摘要纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { auditExportFilename, auditExportBlob, auditExportRows, notifyTestSummary } = require('./audit-export.js');

test('auditExportFilename 格式 audit-YYYYMMDD-HHMMSS.json', () => {
  const name = auditExportFilename(new Date('2026-09-27T08:05:09Z'));
  assert.strictEqual(name, 'audit-20260927-080509.json');
});

test('auditExportFilename 补零（月/日/时/分/秒个位）', () => {
  const name = auditExportFilename(new Date('2026-01-02T03:04:05Z'));
  assert.strictEqual(name, 'audit-20260102-030405.json');
});

test('auditExportBlob 生成 UTF-8 JSON（美化 + 尾换行）', async () => {
  const blob = auditExportBlob([{ action: 'upload', ts: '2026-09-27T00:00:00Z' }]);
  assert.ok(blob instanceof Blob, '应为 Blob');
  assert.strictEqual(blob.type, 'application/json;charset=utf-8');
  const text = await blob.text();
  assert.ok(text.includes('"action": "upload"'));
  assert.ok(text.endsWith('\n'), '尾部换行');
});

test('auditExportBlob 非数组输入归一为空数组', async () => {
  const blob = auditExportBlob(null);
  const text = await blob.text();
  assert.strictEqual(JSON.parse(text).length, 0);
});

test('auditExportRows 返回记录数', () => {
  assert.strictEqual(auditExportRows([{}, {}, {}]), 3);
  assert.strictEqual(auditExportRows(undefined), 0);
});

test('notifyTestSummary 全部成功', () => {
  const s = notifyTestSummary({ email: 'ok', webhook: 'ok' });
  assert.strictEqual(s.ok, true);
  assert.ok(s.message.includes('2 个渠道全部成功'));
});

test('notifyTestSummary 部分失败', () => {
  const s = notifyTestSummary({ email: 'ok', webhook: 'failed: 连接超时' });
  assert.strictEqual(s.ok, false);
  assert.ok(s.message.includes('1 成功 / 1 失败'));
  assert.ok(s.message.includes('webhook'));
});

test('notifyTestSummary 无渠道', () => {
  const s = notifyTestSummary({});
  assert.strictEqual(s.ok, false);
  assert.ok(s.message.includes('无可用渠道'));
});
