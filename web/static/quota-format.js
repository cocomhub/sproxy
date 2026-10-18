/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * quota-format.js —— 配额面板的**纯渲染模块**（与 DOM/网络完全隔离，可 node:test 直测）。
 *
 * 隔离原则（同 user-volumes.js / app-render.js）：
 *   1. 纯函数：输入数据 → HTML 字符串；不碰 DOM、不读模块状态、不碰全局；
 *   2. app.js 只做 DOM 写入与网络流程，凡此处有的函数一律走本模块（quotaFormat.*）；
 *   3. 本文件 UMD：浏览器挂全局 quotaFormat，Node module.exports 可 require。
 *
 * 语义要点（与后端 `GET /api/quota` / `GET /api/volumes` 契约一致，**必须保真**）：
 *   - **未知 ≠ 0**：`has_quota=false` 表示「无权威数据」⇒ 显示「未知」并提示汇总不完整，
 *     绝不渲染成 0（0 会被误读为「已用空」）；
 *   - **上限 0 = 不限**：`unlimited=true`（或 capacity<=0）⇒ 显示「不限」，不参与百分比计算。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.quotaFormat = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  function escHtml(s) {
    return String(s == null ? '' : s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
  }

  // formatSize 与 app-render.formatSize 同语义（GB 档）：<1KB → "N B"，<1MB → KB，
  // <1GB → MB，否则 GB。
  function formatSize(n) {
    if (n == null) return '-';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(2) + ' GB';
  }

  // quotaCell 渲染「已用 / 上限」单元格。三态：
  //   未知（hasQuota===false）→ 「未知」；不限（unlimited 或 capacity<=0）→ 「N / 不限」；
  //   正常 → 「N / M」。
  //
  // incomplete=true ⇒ 该值是**下界**（后端 `Partial=true`：部分卷无权威数据，未知部分未计入）
  // ⇒ 必须标 `≥`，不得当精确值呈现（后端契约：禁当完整值 ✗）。
  function quotaCell(used, capacity, hasQuota, unlimited, incomplete) {
    if (hasQuota === false) return '未知';
    const u = (incomplete === true ? '≥ ' : '') + formatSize(used == null ? 0 : used);
    if (unlimited === true || capacity == null || capacity <= 0) return u + ' / 不限';
    return u + ' / ' + (incomplete === true ? '≥ ' : '') + formatSize(capacity);
  }

  // usagePercent 返回 0..100 的整数百分比；不限/未知/不完整 → null（**不**编造 0，
  // 也不拿**下界**去算比例——那会把「未知」伪装成「占用低」✗）。
  function usagePercent(used, capacity, hasQuota, unlimited, incomplete) {
    if (hasQuota === false || unlimited === true || incomplete === true) return null;
    if (capacity == null || capacity <= 0) return null;
    const p = Math.round(((used == null ? 0 : used) / capacity) * 100);
    return p < 0 ? 0 : (p > 100 ? 100 : p);
  }

  // volumeQuotaTableHtml 渲染「按卷」配额表（数据来自 GET /api/quota 的逐卷明细：
  // `{volume, used, capacity, has_quota}`，与 `pkg/server/quota_federated.go` 契约一致）。
  function volumeQuotaTableHtml(vols) {
    if (!vols || vols.length === 0) {
      return '<div class="empty-msg">暂无可显示的卷。</div>';
    }
    let html = '<table class="quota-table" style="width:100%;border-collapse:collapse;font-size:14px;">';
    html += '<thead><tr style="background:var(--bg-hover);">';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">卷</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">已用 / 上限</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">占用</th>';
    html += '</tr></thead><tbody>';
    for (const v of vols) {
      const pct = usagePercent(v.used, v.capacity, v.has_quota !== false, v.unlimited === true);
      const pctTxt = pct == null ? '—' : pct + '%';
      html += '<tr>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(v.volume) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">'
        + escHtml(quotaCell(v.used, v.capacity, v.has_quota !== false, v.unlimited === true)) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(pctTxt) + '</td>';
      html += '</tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  // federatedQuotaHtml 渲染联邦配额汇总（GET /api/quota）：总计 + 逐卷明细 + 不完整提示。
  // `f` 形状（与 pkg/server/quota_federated.go 契约一致，**顶层无 has_quota** ✗）：
  //   { owner, used, capacity, unlimited, partial, volumes:[{volume,kind,used,capacity,has_quota}] }
  // 语义：used/capacity 是「Σ 有权威卷」= **下界**；`partial=true` ⇒ 含未知部分 ⇒ 必须显式处置。
  function federatedQuotaHtml(f) {
    if (!f) return '<div class="empty-msg">暂无配额数据。</div>';
    const incomplete = f.partial === true;
    let html = '<div class="quota-summary" style="font-size:14px;margin-bottom:8px;">';
    html += '<strong>合计：</strong>'
      + escHtml(quotaCell(f.used, f.capacity, true, f.unlimited === true, incomplete));
    if (incomplete) {
      // 未知 ≠ 0：汇总含未知项 ⇒ 数值是下界（已由 quotaCell 标 ≥），并显式提示。
      html += ' <span class="quota-partial" style="color:var(--text-muted);font-size:12px;">'
        + '（汇总不完整：部分卷无权威数据）</span>';
    }
    html += '</div>';
    html += volumeQuotaTableHtml(f.volumes);
    return html;
  }

  return {
    formatSize: formatSize,
    quotaCell: quotaCell,
    usagePercent: usagePercent,
    volumeQuotaTableHtml: volumeQuotaTableHtml,
    federatedQuotaHtml: federatedQuotaHtml,
  };
});
