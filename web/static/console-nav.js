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

// consoleHashFor 由分区 id 生成深链 hash（如 `#/console/cloud-tasks`）。
function consoleHashFor(tab) {
  const id = consoleTabValid(tab) ? tab : 'stats';
  return '#/console/' + id;
}

// tabFromHash 从 hash 解析分区 id（非控制台 hash ⇒ 空串；非法分区 ⇒ 概览）。
function tabFromHash(hash) {
  const m = /^#\/console\/([a-z0-9-]+)$/.exec(String(hash || ''));
  if (!m) return '';
  return consoleTabValid(m[1]) ? m[1] : 'stats';
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
    consoleNavHtml: consoleNavHtml,
    consoleLayoutStyle: consoleLayoutStyle,
  };
}
