// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// sync-conflicts-format.js WebUI 同步冲突渲染纯函数（B3：/api/sync/conflicts 面板）。
// 服务端 GET /api/sync/conflicts → {conflicts: [{id, path, hunk_count, ours, theirs, ts, resolved}]}；
// resolve: POST /api/sync/conflicts/{id}/resolve body {choice: "ours"|"theirs"|"manual", content?}。
// 纯函数可 node --test，不依赖 DOM。

'use strict';

// syncConflictsHtml(data) → 冲突列表表格（path/hunk 数/双方摘要 + resolve 按钮）。
function syncConflictsHtml(data) {
  var list = (data && data.conflicts) || [];
  if (list.length === 0) {
    return '<div class="empty-msg">暂无未解决冲突</div>';
  }
  var rows = list.map(function (c) {
    return '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(c.path) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' + (c.hunk_count || 0) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' +
      escHtml(c.ours ? c.ours.join(' ⏎ ') : '') +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' +
      escHtml(c.theirs ? c.theirs.join(' ⏎ ') : '') +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' +
      '<button type="button" class="btn btn-sm btn-primary conflict-ours-btn" data-id="' + escHtml(c.id) + '">采用我方</button> ' +
      '<button type="button" class="btn btn-sm btn-secondary conflict-theirs-btn" data-id="' + escHtml(c.id) + '">采用对方</button></td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr style="background:var(--bg-hover);">' +
    '<th style="padding:6px 8px;text-align:left;">路径</th><th style="padding:6px 8px;text-align:center;">Hunk</th>' +
    '<th style="padding:6px 8px;text-align:left;">我方</th><th style="padding:6px 8px;text-align:left;">对方</th>' +
    '<th style="padding:6px 8px;text-align:center;">操作</th></tr></thead><tbody>' + rows + '</tbody></table>';
}

// conflictResolveQuery(id, choice) → resolve 端点 URL。
function conflictResolveQuery(id, choice) {
  return '/api/sync/conflicts/' + encodeURIComponent(id) + '/resolve';
}

function escHtml(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { syncConflictsHtml: syncConflictsHtml, conflictResolveQuery: conflictResolveQuery };
}
