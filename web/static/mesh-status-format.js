// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// mesh-status-format.js WebUI Mesh 状态渲染纯函数（B8：/api/mesh/status 面板）。
// 纯函数可 node --test，不依赖 DOM。

'use strict';

// meshStatusHtml(st) → Mesh 状态面板 HTML（读/写面 + node 角色 + hub/信令）。
function meshStatusHtml(st) {
  var s = st || {};
  var rows = [];
  rows.push(['读面（remote_read）', faceText(s.remote_read)],
    ['写面（remote_write）', faceText(s.remote_write)]);
  if (s.node) {
    rows.push(['节点角色（node）', (s.node.running ? '运行中' : '未运行') + (s.node.node_id ? ' · ' + s.node.node_id : '')],
      ['Hub URL', s.node.hub_url || s.hub_url || '-'],
      ['WebRTC 信令', s.node.webrtc ? '开启' : '关闭']);
    if (s.node.services?.length) rows.push(['声明服务', s.node.services.join(', ')]);
  }
  rows.push(['信令（signaling）', s.signaling_enabled ? '已配置' : '未配置']);
  return tableRowsHtml(rows);
}

// faceText(f) → 面状态文案（enabled/addr/pinned）。
function faceText(f) {
  if (!(f?.enabled)) return '未启用';
  var t = '已启用 · ' + (f.addr || '?');
  if (f.pinned > 0) t += ' · 固定 ' + f.pinned + ' 指纹';
  return t;
}

function tableRowsHtml(rows) {
  return '<table style="width:100%;border-collapse:collapse;font-size:13px;"><tbody>' + rows.map(function (r) {
    return '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);color:var(--text-secondary);white-space:nowrap;">' + escHtml(r[0]) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(r[1]) + '</td></tr>';
  }).join('') + '</tbody></table>';
}

function escHtml(s) {
  return String(s == null ? '' : s)
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { meshStatusHtml: meshStatusHtml, faceText: faceText };
}
