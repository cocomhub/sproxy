// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credentials-format.js WebUI 凭据管理渲染纯函数（B2：/api/credentials 面板）。
// 服务端：GET /api/credentials → {ak: [{ak, owner, sk_count, alive_sk}]}（admin-only）；
// POST /api/credentials body {ak, owner, role, secret?}；DELETE /api/credentials/{ak}。
// 纯函数可 node --test，不依赖 DOM。

'use strict';

// credentialsTableHtml(data) → 凭据列表表格（AK/owner/SK 数/存活 SK + 删除按钮）。
function credentialsTableHtml(data) {
  var list = (data && data.ak) || [];
  if (list.length === 0) {
    return '<div class="empty-msg">暂无凭据</div>';
  }
  var rows = list.map(function (k) {
    return '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-family:monospace;">' + escHtml(k.ak) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(k.owner) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' + (k.sk_count || 0) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' + (k.alive_sk || 0) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' +
      '<button type="button" class="btn btn-sm btn-danger cred-delete-btn" data-ak="' + escHtml(k.ak) + '">删除</button></td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr style="background:var(--bg-hover);">' +
    '<th style="padding:6px 8px;text-align:left;">AK</th><th style="padding:6px 8px;text-align:left;">Owner</th>' +
    '<th style="padding:6px 8px;text-align:center;">SK 数</th><th style="padding:6px 8px;text-align:center;">存活</th>' +
    '<th style="padding:6px 8px;text-align:center;">操作</th></tr></thead><tbody>' + rows + '</tbody></table>';
}

// credentialsFormHtml() → 新增 AK 表单（ak/owner/role + 提交）。
function credentialsFormHtml() {
  return '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:10px;">' +
    '<div style="font-weight:600;margin-bottom:6px;">新增凭据（admin）</div>' +
    '<div style="display:flex;gap:8px;flex-wrap:wrap;">' +
    '<input id="cred-ak" placeholder="AK" style="padding:5px 8px;font-family:monospace;font-size:12px;">' +
    '<input id="cred-owner" placeholder="Owner" style="padding:5px 8px;font-size:12px;">' +
    '<select id="cred-role" style="padding:5px 8px;font-size:12px;"><option value="user">user</option><option value="node">node</option></select>' +
    '<button type="button" id="cred-add-btn" class="btn btn-sm btn-primary">新增</button>' +
    '</div><div id="cred-msg" style="font-size:12px;color:var(--text-muted);margin-top:6px;"></div></div>';
}

// credentialsPanelHtml(data) → 完整凭据面板（列表 + 新增表单）。
function credentialsPanelHtml(data) {
  return credentialsTableHtml(data) + credentialsFormHtml();
}

// credAddBody(ak, owner, role) → POST /api/credentials 请求体（role 空归一 user）。
function credAddBody(ak, owner, role) {
  var body = { ak: ak, owner: owner };
  if (role && role !== 'user') body.role = role;
  return body;
}

function escHtml(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { credentialsTableHtml: credentialsTableHtml, credentialsFormHtml: credentialsFormHtml, credentialsPanelHtml: credentialsPanelHtml, credAddBody: credAddBody };
}
