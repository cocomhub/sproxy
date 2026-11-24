// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 键盘与 a11y：命令面板（⌘K / Ctrl+K）、g 前缀跳转、列表 j/k/x/Enter、
// 快捷键帮助（?）、密度切换、aria-live 播报。纯匹配逻辑可在 Node 下 require 单测。
'use strict';

(function () {
  var THEME_KEY = 'sproxy_v2_theme';
  var DENSITY_KEY = 'sproxy_v2_density';

  function commandItems() {
    return [
      { id: 'nav-files', label: '前往 文件', kind: 'nav', route: 'files', keys: 'files 文件' },
      { id: 'nav-transfer', label: '前往 传输', kind: 'nav', route: 'transfer', keys: 'transfer 传输' },
      { id: 'nav-trash', label: '前往 回收站', kind: 'nav', route: 'trash', keys: 'trash 回收站' },
      { id: 'nav-console', label: '前往 控制台', kind: 'nav', route: 'console', keys: 'console 控制台' },
      { id: 'cs-overview', label: '控制台 · 概览', kind: 'console', cs: 'overview', route: 'console', keys: 'overview 概览 kpi' },
      { id: 'cs-tasks', label: '控制台 · 云端任务', kind: 'console', cs: 'tasks', route: 'console', keys: 'tasks 云端任务' },
      { id: 'cs-issues', label: '控制台 · 待处理事项', kind: 'console', cs: 'issues', route: 'console', keys: 'issues 待处理' },
      { id: 'cs-audit', label: '控制台 · 审计', kind: 'console', cs: 'audit', route: 'console', keys: 'audit 审计' },
      { id: 'cs-config', label: '控制台 · 配置', kind: 'console', cs: 'config', route: 'console', keys: 'config 配置' },
      { id: 'cs-notify', label: '控制台 · 通知', kind: 'console', cs: 'notify', route: 'console', keys: 'notify 通知' },
      { id: 'cs-verify', label: '控制台 · 巡检', kind: 'console', cs: 'verify', route: 'console', keys: 'verify 巡检' },
      { id: 'cs-access', label: '控制台 · 接入', kind: 'console', cs: 'access', route: 'console', keys: 'access 接入' },
      { id: 'act-theme', label: '切换主题（亮/暗）', kind: 'action', act: 'theme', keys: 'theme 主题 亮色 暗色' },
      { id: 'act-density', label: '切换表格密度', kind: 'action', act: 'density', keys: 'density 密度 紧凑' },
      { id: 'act-help', label: '快捷键帮助', kind: 'action', act: 'help', keys: 'help 帮助 快捷键' },
      { id: 'act-legacy', label: '打开旧版界面', kind: 'action', act: 'legacy', keys: 'legacy 旧版' }
    ];
  }

  function filterCommands(items, query) {
    var q = String(query == null ? '' : query).trim().toLowerCase();
    if (!q) { return (items || []).slice(); }
    return (items || []).filter(function (it) {
      return (it.label + ' ' + (it.keys || '')).toLowerCase().indexOf(q) !== -1;
    });
  }

  function clampIndex(index, len) {
    if (!len) { return 0; }
    return Math.max(0, Math.min(len - 1, index));
  }

  function nextIndex(current, len, delta) {
    if (!len) { return 0; }
    return ((current + delta) % len + len) % len;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      commandItems: commandItems, filterCommands: filterCommands,
      nextIndex: nextIndex, clampIndex: clampIndex
    };
  }

  if (typeof document === 'undefined') { return; }

  function el(id) { return document.getElementById(id); }
  function isTyping(t) {
    if (!t) { return false; }
    var tag = (t.tagName || '').toLowerCase();
    return tag === 'input' || tag === 'textarea' || tag === 'select' || t.isContentEditable;
  }

  function announce(text) {
    var live = el('v2-live');
    if (live) { live.textContent = text; }
  }

  function applyDensity(d) {
    var v = d === 'compact' ? 'compact' : 'cozy';
    document.documentElement.setAttribute('data-density', v);
    try { localStorage.setItem(DENSITY_KEY, v); } catch (e) { /* 忽略 */ }
    var btn = el('v2-density-toggle');
    if (btn) { btn.setAttribute('aria-pressed', v === 'compact' ? 'true' : 'false'); }
  }

  function toggleDensity() {
    var cur = document.documentElement.getAttribute('data-density') === 'compact' ? 'cozy' : 'compact';
    applyDensity(cur);
    announce('表格密度：' + (cur === 'compact' ? '紧凑' : '舒适'));
  }

  function toggleTheme() {
    var next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem(THEME_KEY, next); } catch (e) { /* 忽略 */ }
    announce('主题：' + (next === 'dark' ? '暗色' : '亮色'));
  }

  var palette = { open: false, items: [], index: 0, restore: null };

  function renderPalette() {
    var list = el('v2-cmdk-list');
    if (!list) { return; }
    list.textContent = '';
    if (!palette.items.length) {
      var none = document.createElement('div');
      none.className = 'cmdk-empty';
      none.textContent = '没有匹配的命令';
      list.appendChild(none);
      return;
    }
    palette.items.forEach(function (it, i) {
      var row = document.createElement('button');
      row.type = 'button';
      row.className = 'cmdk-item' + (i === palette.index ? ' active' : '');
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', i === palette.index ? 'true' : 'false');
      row.textContent = it.label;
      row.addEventListener('mousedown', function (ev) { ev.preventDefault(); palette.index = i; runCommand(); });
      list.appendChild(row);
    });
    var active = list.children[palette.index];
    if (active && active.scrollIntoView) { active.scrollIntoView({ block: 'nearest' }); }
  }

  function openPalette() {
    if (palette.open) { closePalette(); return; }
    var box = el('v2-cmdk');
    var back = el('v2-cmdk-back');
    if (!box || !back) { return; }
    palette.open = true;
    palette.restore = document.activeElement;
    palette.items = filterCommands(commandItems(), '');
    palette.index = 0;
    back.hidden = false;
    box.hidden = false;
    var input = el('v2-cmdk-input');
    if (input) { input.value = ''; input.focus(); }
    renderPalette();
  }

  function closePalette() {
    var box = el('v2-cmdk');
    var back = el('v2-cmdk-back');
    if (box) { box.hidden = true; }
    if (back) { back.hidden = true; }
    palette.open = false;
    if (palette.restore && palette.restore.focus) { palette.restore.focus(); }
  }

  function goRoute(route) {
    window.location.hash = '#/' + route;
    var node = document.querySelector('[data-route="' + route + '"]');
    if (node) { node.click(); }
    announce('已切换到 ' + route);
  }

  function selectConsoleSection(name) {
    var nav = document.querySelector('[data-cs="' + name + '"]');
    if (nav) { nav.click(); }
    announce('控制台分区：' + name);
  }

  function runCommand() {
    var it = palette.items[palette.index];
    if (!it) { return; }
    if (it.kind === 'nav') { goRoute(it.route); }
    else if (it.kind === 'console') {
      goRoute('console');
      setTimeout(function () { selectConsoleSection(it.cs); }, 0);
    } else if (it.act === 'theme') { toggleTheme(); }
    else if (it.act === 'density') { toggleDensity(); }
    else if (it.act === 'help') { toggleHelp(true); }
    else if (it.act === 'legacy') { window.location.href = '../index.html'; }
    closePalette();
  }

  function toggleHelp(force) {
    var box = el('v2-help');
    if (!box) { return; }
    var show = force === undefined ? box.hidden : force;
    box.hidden = !show;
    if (show) {
      var close = box.querySelector('[data-close-help]');
      if (close) { close.focus(); }
      announce('快捷键帮助已打开');
    }
  }

  function moveRow(delta) {
    var rows = document.querySelectorAll('#v2-file-body tr[data-name]');
    if (!rows.length) { return; }
    var cur = -1;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].classList.contains('row-focus')) { cur = i; break; }
    }
    var next = cur < 0 ? (delta > 0 ? 0 : rows.length - 1) : (cur + delta);
    next = clampIndex(next, rows.length);
    rows.forEach(function (r) { r.classList.remove('row-focus'); r.removeAttribute('tabindex'); });
    rows[next].classList.add('row-focus');
    rows[next].setAttribute('tabindex', '-1');
    rows[next].focus();
    announce('第 ' + (next + 1) + ' 行：' + rows[next].getAttribute('data-name'));
  }

  function focusedRow() {
    return document.querySelector('#v2-file-body tr.row-focus');
  }

  function toggleRowSelection() {
    var row = focusedRow();
    if (!row) { return; }
    var cb = row.querySelector('.cb');
    if (!cb) { return; }
    cb.checked = !cb.checked;
    cb.dispatchEvent(new Event('change', { bubbles: true }));
    announce((cb.checked ? '已选择 ' : '已取消选择 ') + row.getAttribute('data-name'));
  }

  function openRow() {
    var row = focusedRow();
    if (!row) { return; }
    var link = row.querySelector('a[data-preview], td a');
    if (link) { link.click(); }
  }

  var pendingG = 0;

  function onKeyDown(ev) {
    var mod = ev.metaKey || ev.ctrlKey;
    if (mod && (ev.key === 'k' || ev.key === 'K')) {
      ev.preventDefault();
      openPalette();
      return;
    }
    if (palette.open) {
      if (ev.key === 'Escape') { ev.preventDefault(); closePalette(); return; }
      if (ev.key === 'ArrowDown') { ev.preventDefault(); palette.index = nextIndex(palette.index, palette.items.length, 1); renderPalette(); return; }
      if (ev.key === 'ArrowUp') { ev.preventDefault(); palette.index = nextIndex(palette.index, palette.items.length, -1); renderPalette(); return; }
      if (ev.key === 'Enter') { ev.preventDefault(); runCommand(); return; }
      return;
    }
    if (ev.key === 'Escape') {
      var help = el('v2-help');
      if (help && !help.hidden) { toggleHelp(false); announce('快捷键帮助已关闭'); }
      return;
    }
    if (isTyping(ev.target)) { return; }
    if (ev.key === '?') { ev.preventDefault(); toggleHelp(); return; }
    if (ev.key === 'g' || ev.key === 'G') {
      pendingG = Date.now();
      return;
    }
    if (pendingG && Date.now() - pendingG < 900) {
      var map = { f: 'files', t: 'transfer', r: 'trash', c: 'console' };
      var target = map[String(ev.key).toLowerCase()];
      if (target) { ev.preventDefault(); pendingG = 0; goRoute(target); return; }
    }
    if ((window.location.hash || '').indexOf('#/files') === 0) {
      if (ev.key === 'j') { ev.preventDefault(); moveRow(1); return; }
      if (ev.key === 'k') { ev.preventDefault(); moveRow(-1); return; }
      if (ev.key === 'x') { ev.preventDefault(); toggleRowSelection(); return; }
      if (ev.key === 'Enter' && focusedRow()) { ev.preventDefault(); openRow(); return; }
    }
  }

  function init() {
    var density = null;
    try { density = localStorage.getItem(DENSITY_KEY); } catch (e) { density = null; }
    applyDensity(density);

    var dbtn = el('v2-density-toggle');
    if (dbtn) { dbtn.addEventListener('click', toggleDensity); }
    var openBtn = el('v2-cmdk-open');
    if (openBtn) { openBtn.addEventListener('click', openPalette); }

    var input = el('v2-cmdk-input');
    if (input) {
      input.addEventListener('input', function () {
        palette.items = filterCommands(commandItems(), input.value);
        palette.index = 0;
        renderPalette();
      });
    }
    var back = el('v2-cmdk-back');
    if (back) { back.addEventListener('click', closePalette); }
    var helpBack = el('v2-help-back');
    if (helpBack) { helpBack.addEventListener('click', function () { toggleHelp(false); }); }
    var help = el('v2-help');
    if (help) {
      Array.prototype.forEach.call(help.querySelectorAll('[data-close-help]'), function (b) {
        b.addEventListener('click', function () { toggleHelp(false); });
      });
    }
    document.addEventListener('keydown', onKeyDown);
  }

  init();
})();
