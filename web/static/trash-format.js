// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// trash-format.js 回收站列表渲染纯函数（WebUI 回收站视图）。
// 纯函数可 node --test 单测。

'use strict';

// trashTableHtml 渲染回收站条目（trash_rel + 解析原路径 + 恢复按钮）。
function trashTableHtml(entries) {
  if (!entries || entries.length === 0) {
    return '<div style="padding:12px;color:var(--text-muted);">回收站为空</div>';
  }
  var rows = entries.map(function (e) {
    return '<tr><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' +
      (e.name || '') +
      '</td><td style="padding:6px;border-bottom:1px solid var(--border);font-size:12px;">' +
      '<button type="button" class="trash-restore-btn" data-trash-rel="' + (e.trash_rel || '') + '" style="font-size:12px;cursor:pointer;">恢复</button>' +
      '</td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;"><tr><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">原路径</th><th style="text-align:left;padding:6px;font-size:12px;color:var(--text-muted);">操作</th></tr>' + rows + '</table>';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { trashTableHtml: trashTableHtml };
}
