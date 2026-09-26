// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-export.js WebUI 审计导出纯函数（B5：/api/audit/export 下载按钮）。
// 纯函数可 node --test 单测，不依赖 DOM。导出契约：服务端 /api/audit/export
// 返回审计事件 JSON 数组（时间正序）；浏览器侧构造 Blob 下载 audit-<ts>.json。

'use strict';

// auditExportFilename(now) → 导出文件名（audit-YYYYMMDD-HHMMSS.json，UTC 时间避免时区偏移）。
function auditExportFilename(now) {
  var d = now || new Date();
  function p(n) { return (n < 10 ? '0' : '') + n; }
  return 'audit-' + d.getUTCFullYear() + p(d.getUTCMonth() + 1) + p(d.getUTCDate()) +
    '-' + p(d.getUTCHours()) + p(d.getUTCMinutes()) + p(d.getUTCSeconds()) + '.json';
}

// auditExportBlob(events) → Blob（UTF-8 JSON 美化 + 尾部换行；供浏览器下载）。
function auditExportBlob(events) {
  var text = JSON.stringify(Array.isArray(events) ? events : [], null, 2) + '\n';
  return new Blob([text], { type: 'application/json;charset=utf-8' });
}

// auditExportRows(events) → 导出预览摘要（首行 = 记录数，供 toast）。
function auditExportRows(events) {
  return Array.isArray(events) ? events.length : 0;
}

// notifyTestSummary(results) → 通知测试结果摘要（{ok, message}）。
// results: {channel: "ok"} 或 {channel: "failed: <err>"}。空对象 = 无渠道可测。
function notifyTestSummary(results) {
  var entries = results || {};
  var names = Object.keys(entries);
  if (names.length === 0) {
    return { ok: false, message: '通知测试：无可用渠道' };
  }
  var failed = names.filter(function (n) { return /^failed:/.test(entries[n]); });
  var message = failed.length === 0
    ? '通知测试：' + names.length + ' 个渠道全部成功'
    : '通知测试：' + (names.length - failed.length) + ' 成功 / ' + failed.length + ' 失败（' + failed[0] + '）';
  return { ok: failed.length === 0, message: message };
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { auditExportFilename: auditExportFilename, auditExportBlob: auditExportBlob, auditExportRows: auditExportRows, notifyTestSummary: notifyTestSummary };
}
