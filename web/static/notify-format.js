// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// notify-format.js 通知历史渲染纯函数（WebUI stats 面板用）。
// 与 app-render.js 同模式：纯函数可 node --test 单测，不依赖 DOM。

'use strict';

// notifyTableHtml 渲染通知历史表格（最近在前）。
// entries: [{ts, action, object, channel, status}]；空 → 空提示行。
function notifyTableHtml(entries) {
  if (!entries || entries.length === 0) {
    return '<div style="padding:12px;color:var(--text-muted);">暂无通知记录</div>';
  }
  var rows = entries.slice(0, 10).map(function (e) {
    var ts = e.ts || '';
    var statusClass = e.status === 'failed' ? 'color:red;' : (e.status === 'debounced' ? 'color:var(--text-muted);' : 'color:var(--text-primary);');
    return '<tr><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' + ts +
      '</td><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' + (e.action || '') +
      '</td><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' + (e.object || '') +
      '</td><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' + (e.channel || '') +
      '</td><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;' + statusClass + '">' + (e.status || '') + '</td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;"><tr><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">时间</th><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">事件</th><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">对象</th><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">渠道</th><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">状态</th></tr>' + rows + '</table>';
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { notifyTableHtml: notifyTableHtml };
}
