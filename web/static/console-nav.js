// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// console-nav.js 控制台（原「服务器监控」弹窗）导航纯函数（2026-11-22）。
//
// 背景：原弹窗固定 500px 宽，内容多（审计/云端任务/待处理事项）时体验极差。
// 改为**全屏控制台**：左侧分区导航 + 右侧大内容区，并支持深链（刷新保持分区）。
// 本文件只做纯函数（可 node --test），DOM/CSS 在 index.html / app.js。

'use strict';

// CONSOLE_TABS 是分区定义（id 与既有 `*-tab` / `*-panel` 一一对应，保持零回归）。
const CONSOLE_TABS = [
  { id: 'stats', label: '概览' },
  { id: 'cloud-tasks', label: '云端任务' },
  { id: 'issues', label: '待处理事项' },
  { id: 'audit', label: '审计' },
  { id: 'volumes', label: '卷' },
  { id: 'vol-manage', label: '卷管理' },
  { id: 'credentials', label: '凭据' },
  { id: 'secret', label: 'Secret' },
  { id: 'sync', label: '同步冲突' },
  { id: 'mesh', label: 'Mesh' },
  { id: 'hub', label: 'Hub' },
  { id: 'config', label: '配置' },
];

// consoleTabIDs 返回全部合法分区 id（供校验/测试）。
function consoleTabIDs() {
  return CONSOLE_TABS.map(function (t) { return t.id; });
}

// consoleTabValid 判断分区 id 是否合法（未知 id 一律回落概览，避免深链把界面搞成空白）。
function consoleTabValid(id) {
  return consoleTabIDs().indexOf(id) !== -1;
}

// consoleHashFor 由分区 id + 可选过滤参数生成深链 hash。
//
// 带参数（2026-11-22）：`#/console/cloud-tasks?failed=1&cls=throttled&volume=baidu&q=xxx`
// ⇒ 分享链接/刷新后仍是"同一视图"，避免用户重复设置筛选。
// 空值参数自动省略（保持 URL 简短、幂等）。
function consoleHashFor(tab, params) {
  const id = consoleTabValid(tab) ? tab : 'stats';
  const p = params || {};
  const q = [];
  Object.keys(p).sort().forEach(function (k) {
    const v = p[k];
    if (v === undefined || v === null || v === '' || v === false || v === 0) return;
    q.push(encodeURIComponent(k) + '=' + encodeURIComponent(v === true ? '1' : String(v)));
  });
  return '#/console/' + id + (q.length ? '?' + q.join('&') : '');
}

// consoleStateFromHash 解析完整深链状态：{tab, params}（非控制台 hash ⇒ tab 为空串）。
// params 值：`1`/`true` ⇒ true，纯数字 ⇒ number，其余为字符串（与 consoleHashFor 对称）。
function consoleStateFromHash(hash) {
  const raw = String(hash || '');
  const m = /^#\/console\/([a-z0-9-]+)(?:\?([^#]*))?$/.exec(raw);
  if (!m) return { tab: '', params: {} };
  const tab = consoleTabValid(m[1]) ? m[1] : 'stats';
  const params = {};
  if (m[2]) {
    m[2].split('&').forEach(function (kv) {
      if (!kv) return;
      const idx = kv.indexOf('=');
      const k = decodeURIComponent(idx < 0 ? kv : kv.slice(0, idx));
      const v = idx < 0 ? '' : decodeURIComponent(kv.slice(idx + 1));
      if (!k) return;
      if (v === '1' || v === 'true') params[k] = true;
      else if (/^\d+$/.test(v)) params[k] = Number(v);
      else params[k] = v;
    });
  }
  return { tab: tab, params: params };
}

// tabFromHash 从 hash 解析分区 id（非控制台 hash ⇒ 空串；非法分区 ⇒ 概览）。
function tabFromHash(hash) {
  const m = /^#\/console\/([a-z0-9-]+)$/.exec(String(hash || ''));
  if (!m) return '';
  return consoleTabValid(m[1]) ? m[1] : 'stats';
}

// consoleNeighborTab 返回相邻分区 id（delta=±1；循环）。
// 用于键盘导航（Alt+←/→）：在控制台内快速前后切换，不依赖鼠标。
function consoleNeighborTab(current, delta) {
  const ids = consoleTabIDs();
  const i = ids.indexOf(current);
  if (i < 0) return ids[0];
  const n = ids.length;
  return ids[((i + (Number(delta) || 1)) % n + n) % n];
}

// consoleTabByOrdinal 返回第 n 个分区（1-based；越界 ⇒ 空串）。
// 用于 Alt+1..9 直接跳转。
function consoleTabByOrdinal(n) {
  const idx = Number(n) - 1;
  const ids = consoleTabIDs();
  return idx >= 0 && idx < ids.length ? ids[idx] : '';
}

// consoleShortcuts 是控制台快捷键清单（单一事实源：帮助面板与文档一致）。
const CONSOLE_SHORTCUTS = [
  { key: '?', desc: '显示/隐藏本帮助' },
  { key: 'Esc', desc: '关闭控制台' },
  { key: 'Alt+← / Alt+→', desc: '切换上一个/下一个分区' },
  { key: 'Alt+1…9', desc: '直接跳到第 N 个分区' },
  { key: '/', desc: '在云端任务分区聚焦关键字搜索' },
];

// consoleHelpHtml 渲染快捷键帮助面板（纯函数，便于单测）。
function consoleHelpHtml() {
  let rows = '';
  CONSOLE_SHORTCUTS.forEach(function (s) {
    rows += '<tr><td style="padding:4px 10px 4px 0;font-family:monospace;white-space:nowrap;">' +
      s.key + '</td><td style="padding:4px 0;">' + s.desc + '</td></tr>';
  });
  return '<div id="console-help" style="position:absolute;right:16px;top:52px;z-index:5;' +
    'background:var(--modal-bg);border:1px solid var(--border);border-radius:8px;padding:12px 14px;' +
    'box-shadow:0 8px 24px rgba(0,0,0,.18);font-size:12px;">' +
    '<div style="font-weight:600;margin-bottom:6px;">键盘快捷键</div>' +
    '<table>' + rows + '</table></div>';
}

// consoleNavHtml 生成左侧导航 HTML（当前分区高亮；计数徽标可选）。
function consoleNavHtml(active, badges) {
  const cur = consoleTabValid(active) ? active : 'stats';
  const b = badges || {};
  let html = '';
  CONSOLE_TABS.forEach(function (t) {
    const on = t.id === cur;
    const n = b[t.id];
    const badge = (n && n > 0)
      ? '<span style="margin-left:auto;background:#c0392b;color:#fff;border-radius:9px;padding:0 7px;font-size:11px;line-height:18px;">' + n + '</span>'
      : '';
    html += '<button type="button" class="console-nav-item' + (on ? ' active' : '') +
      '" data-console-tab="' + t.id + '" style="display:flex;align-items:center;gap:8px;width:100%;text-align:left;' +
      'padding:9px 12px;border:none;border-radius:6px;cursor:pointer;font-size:13px;background:' +
      (on ? 'var(--tab-active-bg,rgba(31,111,235,.12))' : 'transparent') + ';color:' +
      (on ? 'var(--text-primary)' : 'var(--text-secondary)') + ';font-weight:' + (on ? '600' : '400') + ';">' +
      t.label + badge + '</button>';
  });
  return html;
}

// consoleLayoutStyle 返回控制台容器尺寸（全屏化：宽 96vw、高 92vh，带上下限）。
// 抽成纯函数便于单测（避免在测试里断言内联样式字符串）。
function consoleLayoutStyle(viewportWidth, viewportHeight) {
  const vw = Number(viewportWidth) || 1280;
  const vh = Number(viewportHeight) || 800;
  const width = Math.max(720, Math.min(1600, Math.round(vw * 0.96)));
  const height = Math.max(520, Math.round(vh * 0.92));
  return { width: width + 'px', height: height + 'px' };
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    CONSOLE_TABS: CONSOLE_TABS,
    consoleTabIDs: consoleTabIDs,
    consoleTabValid: consoleTabValid,
    consoleHashFor: consoleHashFor,
    tabFromHash: tabFromHash,
    consoleStateFromHash: consoleStateFromHash,
    consoleNavHtml: consoleNavHtml,
    CONSOLE_SHORTCUTS: CONSOLE_SHORTCUTS,
    consoleHelpHtml: consoleHelpHtml,
    consoleNeighborTab: consoleNeighborTab,
    consoleTabByOrdinal: consoleTabByOrdinal,
    consoleLayoutStyle: consoleLayoutStyle,
  };
}
