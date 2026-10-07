// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-rows-format.js —— WebUI「任务审计」渲染纯函数（传输页云任务行「审计」弹窗）。
// 服务端数据源：CloudTask.audit → []audit.Row（SnapshotTask 自动携带）：
//   Row{type, level, dim, step, start_ms, dur_ms, err, bw_bps, bytes, meta:{cipher_bytes?,algorithm?,integrity_status?,integrity_sames?,integrity_decision?}, node}
// 渲染口径（spec）：
//   - 通用列：type / level / step / dur_ms / bytes / bw_bps；
//   - 加密行（type==='encrypt'）并排「明文 bytes」 vs 「密文 meta.cipher_bytes」+ algorithm（放「说明」列）；
//   - download 行：dur_ms / bytes / bw_bps；完整性判定（meta.integrity_status + integrity_sames，
//     非空才显示；verified/damaged/unverified 中文文案，damaged ×N 附重下一致次数）进「说明」列；
//   - err（异常说明）与 algorithm / node 归入「说明」列。
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

// _integrityStatusText(m)：download 行完整性判定文案（B5-I1——审计面板渲染完整性状态，
// 承接后端 download span Meta 写入的 integrity_status/integrity_sames，见
// pkg/cloud/manager_task.go runDownloadWithAudit）。
// integrity_status ∈ {"", "verified", "damaged", "unverified"}（服务端固定枚举）：
// damaged → 完整性异常；unverified → 未校验；verified → 已验证；未知值原样转义（防注入）。
// integrity_sames（重下一致次数）>0 时附 ×N。status 非空才渲染（verified 也展示，供溯源）。
function _integrityStatusText(m) {
  const st = m.integrity_status;
  let label;
  if (st === 'damaged') label = '完整性异常';
  else if (st === 'unverified') label = '未校验';
  else if (st === 'verified') label = '已验证';
  else label = String(st);
  let txt = '完整性: ' + appRender.escHtml(label);
  const same = m.integrity_sames;
  if (typeof same === 'number' && same > 0) txt += ' ×' + same;
  return txt;
}

// _detailText(r) → 「说明」列：encrypt 行 algorithm；download 行完整性判定；err；node；空 → ''。
// 完整性字段非空才渲染（damaged ×N 附重下一致次数）；多段用「；」连接（如 damaged 行同时
// 带 err 时 integrity 与 err 并存）。
function _detailText(r) {
  const m = r && r.meta;
  const parts = [];
  if (r && r.type === 'encrypt' && m && m.algorithm) {
    parts.push('算法: ' + appRender.escHtml(String(m.algorithm)));
  }
  if (r && r.type === 'download' && m && m.integrity_status) {
    parts.push(_integrityStatusText(m));
  }
  if (r && r.err) parts.push(appRender.escHtml(String(r.err)));
  if (r && r.node) parts.push('node: ' + appRender.escHtml(String(r.node)));
  return parts.join('；');
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