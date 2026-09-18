/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * user-volumes.js —— 用户卷面板纯渲染模块（与 DOM/网络完全隔离，可 node:test 直测）。
 *
 * 隔离原则（同 app-render.js）：
 *   1. 纯函数生成 HTML 字符串 / 解析，不碰 DOM、不读模块状态、不碰全局；
 *   2. app.js 只做 DOM 写入与网络/流程，凡此处有的函数一律走本模块（userVolumes.*）；
 *   3. 本文件 UMD：浏览器挂全局 userVolumes，Node module.exports 可 require。
 *      顶层无任何副作用（不引用 document/window/localStorage）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.userVolumes = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // ---- 基础工具（纯，本模块内聚） ----
  function escHtml(s) {
    return String(s == null ? '' : s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
  }

  // 与 app-render.formatSize 同语义（GB 档）：<1KB → "N B"；<1MB → KB；<1GB → MB；否则 GB。
  function formatSize(n) {
    if (n == null) return '-';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(2) + ' GB';
  }

  // ---- 用户卷表格（GET /api/volumes/user → 列表渲染） ----
  // 列：卷名 / 类型 / 容量 / 操作（删除）。owner 由服务端按认证派生，客户端不显示。
  function userVolumesTableHtml(vols) {
    if (!vols || vols.length === 0) {
      return '<div class="empty-msg">暂无用户卷，点击上方「创建用户卷」添加。</div>';
    }
    let html = '<table style="width:100%;border-collapse:collapse;font-size:14px;">';
    html += '<thead><tr style="background:var(--bg-hover);">';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">卷名</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">类型</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">容量</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">已用</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid var(--border-color);">操作</th>';
    html += '</tr></thead><tbody>';
    for (const v of vols || []) {
      const capTxt = v.capacity > 0 ? formatSize(v.capacity) : '不限';
      const usageTxt = (v.usage || 0) > 0 ? formatSize(v.usage) : '0';
      html += '<tr>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-weight:600;">' + escHtml(v.name) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' + escHtml(v.type || '-') + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + capTxt + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' + usageTxt + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">';
      html += '<button type="button" class="btn btn-danger btn-sm" data-action="delete-user-volume" data-name="' + escHtml(v.name) + '" style="padding:2px 8px;font-size:12px;">删除</button>';
      html += '</td>';
      html += '</tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  // ---- 创建用户卷表单（POST /api/volumes/user 前置交互） ----
  // types = 已注册 backend 类型数组（服务端 /api/volumes 无该列表时用默认 ['baidupcs']）。
  // backendOptionsHtml 渲染 <option> 列表（V4 动态下拉：/api/backends 返回类型填充）。
  // 空列表 → 空字符串（调用方保留静态默认下拉）。
  function backendOptionsHtml(types) {
    const t = types || [];
    return t.map(function (x) { return '<option value="' + escHtml(x) + '">' + escHtml(x) + '</option>'; }).join('');
  }

  function createUserVolumeFormHtml(types) {
    const t = types && types.length ? types : ['baidupcs'];
    const opts = t.map(function (x) { return '<option value="' + escHtml(x) + '">' + escHtml(x) + '</option>'; }).join('');
    return '<div style="margin-bottom:12px;padding:12px;border:1px solid var(--border-color);border-radius:6px;">'
      + '<div style="font-weight:600;margin-bottom:8px;">创建用户卷</div>'
      + '<div style="display:flex;flex-wrap:wrap;gap:8px;align-items:center;">'
      + '<input type="text" id="uv-name" placeholder="卷名（唯一）" style="padding:6px 8px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-input, #fff);color:var(--text-primary);" />'
      + '<select id="uv-type" style="padding:6px 8px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-input, #fff);color:var(--text-primary);">' + opts + '</select>'
      + '<input type="text" id="uv-capacity" placeholder="容量（如 100GiB，留空不限）" style="padding:6px 8px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-input, #fff);color:var(--text-primary);" />'
      + '<input type="text" id="uv-extra" placeholder="extra JSON（如 {&quot;bduss&quot;:&quot;...&quot;}）" style="padding:6px 8px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-input, #fff);color:var(--text-primary);flex:1 1 220px;" />'
      + '<button type="button" id="uv-create-btn" class="btn btn-primary" style="padding:6px 12px;">创建</button>'
      + '</div>'
      + '<div id="uv-create-msg" style="font-size:12px;color:var(--text-muted);margin-top:6px;"></div>'
      + '</div>';
  }

  // ---- extra JSON 解析（--extra / Web 表单共用语义） ----
  // 返回 {error: <msg>} 表示非法（非 JSON / 非对象）；合法返回解析后的对象（空串 → {}）。
  function parseExtra(jsonStr) {
    const s = String(jsonStr == null ? '' : jsonStr).trim();
    if (s === '') return {};
    let parsed;
    try {
      parsed = JSON.parse(s);
    } catch (e) {
      return { error: 'extra 必须是合法 JSON 对象: ' + e.message };
    }
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return { error: 'extra 必须是 JSON 对象（非数组/标量）' };
    }
    return parsed;
  }

  return {
    userVolumesTableHtml,
    createUserVolumeFormHtml,
    backendOptionsHtml,
    parseExtra,
  };
});
