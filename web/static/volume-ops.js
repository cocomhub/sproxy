// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume-ops.js WebUI 卷操作纯函数（B1：copy/move/rebalance 操作按钮）。
// 对应服务端 POST /api/volumes/{copy,move,rebalance}（query 参数 from_volume/to_volume/filename/max_bytes）。
// 纯函数可 node --test，不依赖 DOM。

'use strict';

// volumeOpsBarHtml(vols, ctx) → 卷操作条 HTML（每卷行内 copy/move 目标下拉 + rebalance 源选择）。
// vols: [{name, mode, capacity, usage, allowed}]；ctx: {mode, maxBytes}（模式标注 + 迁移上限）。
function volumeOpsBarHtml(vols) {
  var list = Array.isArray(vols) ? vols : [];
  if (list.length < 2) {
    return '<div style="padding:8px;color:var(--text-muted);font-size:12px;">至少两个卷才能进行复制/移动/再平衡操作</div>';
  }
  var options = list.map(function (v) {
    return '<option value="' + escapeAttr(v.name) + '">' + escapeAttr(v.name) + '</option>';
  }).join('');
  var rows = list.map(function (v) {
    return '<tr><td style="padding:4px 8px;font-weight:600;">' + escapeAttr(v.name) + '</td>' +
      '<td style="padding:4px 8px;">' +
      '<select class="vol-op-target" data-vol="' + escapeAttr(v.name) + '">' + options + '</select>' +
      ' <button type="button" class="btn btn-sm btn-secondary vol-copy-btn" data-vol="' + escapeAttr(v.name) + '">复制</button>' +
      ' <button type="button" class="btn btn-sm btn-secondary vol-move-btn" data-vol="' + escapeAttr(v.name) + '">移动</button>' +
      '</td>' +
      '<td style="padding:4px 8px;"><button type="button" class="btn btn-sm btn-warning vol-rebalance-btn" data-vol="' + escapeAttr(v.name) + '">再平衡</button></td></tr>';
  }).join('');
  return '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px;">' +
    '<div style="font-weight:600;margin-bottom:6px;">卷操作</div>' +
    '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr style="color:var(--text-muted);font-size:12px;">' +
    '<th style="text-align:left;padding:4px 8px;">源卷</th><th style="text-align:left;padding:4px 8px;">目标</th><th style="text-align:left;padding:4px 8px;">再平衡</th></tr></thead><tbody>' +
    rows + '</tbody></table></div>';
}

// volumeOpQuery(op, fromVol, toVol, filename, maxBytes) → copy/move/rebalance 的 query 串。
function volumeOpQuery(op, fromVol, toVol, filename, maxBytes) {
  var parts = ['from_volume=' + encodeURIComponent(fromVol), 'to_volume=' + encodeURIComponent(toVol)];
  if (op === 'copy' || op === 'move') {
    if (filename) parts.push('filename=' + encodeURIComponent(filename));
  } else if (op === 'rebalance' && maxBytes > 0) {
    parts.push('max_bytes=' + maxBytes);
  }
  return '/api/volumes/' + op + '?' + parts.join('&');
}

// volumeOpLabel(op) → 操作中文标签。
function volumeOpLabel(op) {
  return op === 'copy' ? '复制' : op === 'move' ? '移动' : op === 'rebalance' ? '再平衡' : op;
}

// escapeAttr HTML 属性转义（防卷名注入）。
function escapeAttr(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// 导出（node --test 用）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { volumeOpsBarHtml: volumeOpsBarHtml, volumeOpQuery: volumeOpQuery, volumeOpLabel: volumeOpLabel, escapeAttr: escapeAttr };
}
