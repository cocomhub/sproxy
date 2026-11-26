// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 回收站：GET /api/trash（多卷汇总）、POST /api/trash/restore、POST /api/trash/empty。
// 条目 trash_rel 是恢复所需路径；volume 用于跨卷定位。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-trash-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function normalizeEntries(body) {
    var list = body && Array.isArray(body.entries) ? body.entries : [];
    return list.filter(Boolean).map(function (e) {
      return { rel: e.trash_rel || '', name: e.name || e.trash_rel || '', volume: e.volume || '' };
    });
  }

  function render(entries) {
    var tbody = el('v2-trash-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    entries.forEach(function (e) {
      var tr = document.createElement('tr');
      var name = document.createElement('td');
      name.className = 'mono'; name.textContent = e.name; name.title = e.rel;
      tr.appendChild(name);
      var vol = document.createElement('td');
      if (e.volume) {
        var pill = document.createElement('span');
        pill.className = 'pill pill-info'; pill.textContent = e.volume;
        vol.appendChild(pill);
      } else { vol.textContent = '—'; vol.className = 'muted'; }
      tr.appendChild(vol);
      var act = document.createElement('td');
      act.className = 'r actions';
      var btn = document.createElement('button');
      btn.type = 'button'; btn.className = 'btn btn-ghost btn-sm'; btn.textContent = '恢复';
      btn.addEventListener('click', function () { restore(e); });
      act.appendChild(btn);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var hint = el('v2-trash-empty-hint');
    if (hint) { hint.hidden = entries.length > 0; }
    var count = el('v2-trash-count');
    if (count) { count.textContent = entries.length + ' 项'; }
    try {
      document.dispatchEvent(new CustomEvent('v2:badge', { detail: { route: 'trash', count: entries.length } }));
      document.dispatchEvent(new CustomEvent('v2:subtitle', {
        detail: { route: 'trash', text: '/ ' + entries.length + ' 个可恢复文件' }
      }));
    } catch (e) { /* 忽略 */ }
  }

  function load() {
    if (!api) { return Promise.resolve(); }
    banner('');
    return api.json('GET', '/api/trash')
      .then(function (body) { render(normalizeEntries(body)); })
      .catch(function (e) { render([]); banner('回收站读取失败：' + (e.message || e), 'warn'); });
  }

  function restore(entry) {
    var q = '?file=' + encodeURIComponent(entry.rel);
    if (entry.volume) { q += '&volume=' + encodeURIComponent(entry.volume); }
    api.json('POST', '/api/trash/restore' + q, {})
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已恢复 ' + entry.name); }
        load();
        if (window.v2filesView) { window.v2filesView.load(); }
      })
      .catch(function (e) { banner('恢复失败：' + (e.message || e), 'warn'); });
  }

  function emptyAll() {
    if (!window.confirm('清空全部卷的回收站？此操作不可撤销。')) { return; }
    api.json('POST', '/api/trash/empty?all=1', {})
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '回收站已清空'); }
        load();
      })
      .catch(function (e) { banner('清空失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var r = el('v2-trash-refresh'); if (r) { r.addEventListener('click', load); }
    var e = el('v2-trash-empty'); if (e) { e.addEventListener('click', emptyAll); }
    if (el('v2-trash-body')) { load(); }
  }

  function maybeInit() {
    if ((window.location.hash || '').indexOf('#/trash') === 0) { init(); }
  }

  if (typeof window !== 'undefined') {
    window.v2trash = { init: init, load: load };
    window.addEventListener('hashchange', maybeInit);
    maybeInit();
  }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeEntries: normalizeEntries };
  }
})();
