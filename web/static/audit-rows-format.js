// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-rows-format.js —— WebUI「任务审计」渲染纯函数（传输页云任务行「审计」弹窗）。
// 服务端数据源：CloudTask.audit → []audit.Row（SnapshotTask 自动携带）：
//   Row{type, level, dim, step, start_ms, dur_ms, err, bw_bps, bytes, meta:{cipher_bytes?,algorithm?}, node_id}
// 渲染口径（spec）：
//   - 通用列：type / level / step / dur_ms / bytes / bw_bps；
//   - 加密行（type==='encrypt'）并排「明文 bytes」 vs 「密文 meta.cipher_bytes」+ algorithm（放「说明」列）；
//   - download/transfer 行显示 dur_ms / bytes / bw_bps；
//   - err（异常说明）与 algorithm / node_id 归入「说明」列。
// 纯函数可 node --test，不依赖 DOM。样式统一 var(--…) 禁内联亮色 hex；HTML 转义复用
// appRender.escHtml（禁自建 escHtml）；字节大小复用 appRender.formatSize（禁自建 formatSize）。
// 非 UMD：顶层 function + 底部 module.exports（Node require），浏览器靠 script 顺序挂全局。
// global: appRender（app-render.js，转义/格式化）

'use strict';

// _num(n)：null/undefined → '-'；否则原样字符串（审计遥测保整数精度，不 formatSize）。
function _num(n) {
  return n == null ? '-' : String(n);
}

// _sizeTxt(n)：字节体积 → appRender.formatSize（复用共享实现）；null/undefined → '-'。
function _sizeTxt(n) {
  return n == null ? '-' : appRender.formatSize(n);
}

// _escHtml(s)：复用 appRender.escHtml（禁自建）。
function _escHtml(s) {
  return appRender.escHtml(s);
}

// _detailText(r) → 「说明」列：encrypt 行优先 algorithm；否则 err；再退 node_id；空 → ''。
function _detailText(r) {
  const m = r && r.meta;
  if (r && r.type === 'encrypt' && m && m.algorithm) {
    return '算法: ' + appRender.escHtml(String(m.algorithm));
  }
  if (r && r.err) return appRender.escHtml(String(r.err));
  if (r && r.node_id) return 'node: ' + appRender.escHtml(String(r.node_id));
  return '';
}

// _cell(content) → 单个 <td>（共享样式 token，禁内联亮色）。
function _cell(content) {
  return '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + content + '</td>';
}

// _th(text) → 单个表头 <th>。
function _th(text) {
  return '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">' + text + '</th>';
}

// auditRowsHtml(opts) → 审计行表格 HTML 字符串。
// opts.rows: audit.Row[]（缺省/空数组 → 占位文案）；全部字段按需可缺省。
function auditRowsHtml(opts) {
  const o = opts || {};
  const rows = Array.isArray(o.rows) ? o.rows : [];
  if (rows.length === 0) return '<div class="empty-msg">暂无审计记录</div>';
  let h = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
  h += '<thead><tr style="background:var(--bg-hover);">' +
    _th('类型') + _th('级别') + _th('步骤') + _th('耗时') + _th('数据') + _th('带宽') + _th('说明') +
    '</tr></thead><tbody>';
  rows.forEach(function (r) {
    r = r || {};
    h += '<tr>';
    h += _cell(_escHtml(r.type || '-'));
    h += _cell(_escHtml(r.level || '-'));
    h += _cell(_escHtml(r.step || r.type || '-'));
    h += _cell(_num(r.dur_ms) === '-' ? '-' : _num(r.dur_ms) + ' ms');
    // 数据列：encrypt 行并排明文 bytes vs 密文 meta.cipher_bytes；其余行显示 bytes。
    if (r.type === 'encrypt' && r.meta && r.meta.cipher_bytes != null) {
      h += _cell('<span style="color:var(--text-secondary);">明文 </span>' + _num(r.bytes) +
        '<span style="color:var(--text-secondary);"> → 密文 </span>' + _num(r.meta.cipher_bytes));
    } else {
      h += _cell(_num(r.bytes));
    }
    // 带宽列：bw_bps 经 formatSize 人类可读。
    h += _cell(r.bw_bps == null || r.bw_bps === '' ? '-' : _sizeTxt(r.bw_bps) + '/s');
    h += _cell(_detailText(r) || '');
    h += '</tr>';
  });
  h += '</tbody></table>';
  return h;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { auditRowsHtml: auditRowsHtml };
}