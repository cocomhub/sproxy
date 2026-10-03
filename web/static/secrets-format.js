// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// secrets-format.js WebUI secret 卷管理渲染纯函数 + 交互辅助（secret 面板）。
// 服务端：POST /api/secrets {name, mode, value?, origin?}；GET /api/secrets →
// {secrets:[...]}；GET /api/secrets/{name} → {name, value}；DELETE /api/secrets/{name}。
// 纯函数可 node --test，不依赖 DOM。
//
// 安全边界（web 只做随机 secret，不做双口令派生——浏览器 WebCrypto 无 scrypt 原生支持）：
//   - 创建 = 服务端生成随机（mode=random），响应返回 secret 值供立即备份（仅本次显示）；
//   - 导出 = GET 明文 hex，展示给用户自行保管（提示下载/复制）；
//   - 原始口令/双口令派生**不在 web 实现**（仅 CLI）。

'use strict';

// secretsTableHtml(data) → secret 列表表格（名称 + 导出/删除操作）。
// data: {secrets: [string]}。
function secretsTableHtml(data) {
  const list = (data && data.secrets) || [];
  if (list.length === 0) {
    return '<div class="empty-msg">暂无 secret（点下方「创建随机」生成）</div>';
  }
  const rows = list.map(function (name) {
    return '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-family:monospace;">' + escHtml(name) +
      '</td><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' +
      '<button type="button" class="btn btn-sm secret-export-btn" data-name="' + escHtml(name) + '">导出</button> ' +
      '<button type="button" class="btn btn-sm btn-danger secret-delete-btn" data-name="' + escHtml(name) + '">删除</button></td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr style="background:var(--bg-hover);">' +
    '<th style="padding:6px 8px;text-align:left;">名称</th><th style="padding:6px 8px;text-align:center;">操作</th></tr></thead>' +
    '<tbody>' + rows + '</tbody></table>';
}

// secretsFormHtml() → 创建随机 secret 表单（name 输入 + 提交）。
function secretsFormHtml() {
  return '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:10px;">' +
    '<div style="font-weight:600;margin-bottom:6px;">创建随机 secret（服务端生成）</div>' +
    '<div style="display:flex;gap:8px;flex-wrap:wrap;">' +
    '<input id="secret-new-name" placeholder="secret 名（如 myvault）" style="padding:5px 8px;font-family:monospace;font-size:12px;">' +
    '<button type="button" id="secret-add-btn" class="btn btn-sm btn-primary">创建</button>' +
    '</div><div id="secret-msg" style="font-size:12px;color:var(--text-muted);margin-top:6px;"></div></div>';
}

// secretsPanelHtml(data) → 完整 secret 面板（列表 + 创建表单）。
function secretsPanelHtml(data) {
  return secretsTableHtml(data) + secretsFormHtml();
}

// secretExportValue(apiValue) → 从 GET /api/secrets/{name} 响应提取明文 secret。
// 兼容 {name, value} 与 {name, value, origin}。
function secretExportValue(resp) {
  return (resp && resp.value) || '';
}

// isSecretHex(value) → 判定是否 64 位小写 hex（与 Go isSecretHex 对齐；web 只读展示）。
function isSecretHex(value) {
  if (typeof value !== 'string' || value.length !== 64) return false;
  return /^[0-9a-f]{64}$/.test(value);
}

function escHtml(s) {
  return String(s == null ? '' : s)
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    secretsTableHtml: secretsTableHtml,
    secretsFormHtml: secretsFormHtml,
    secretsPanelHtml: secretsPanelHtml,
    secretExportValue: secretExportValue,
    isSecretHex: isSecretHex,
    escHtml: escHtml,
  };
}
