// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台通知分区：历史（GET /api/notify/history）+ 渠道自检（POST /api/notify/test）。
// 未启用 notify 时服务端返回 400「notify 未启用」⇒ 分区展示降级提示，不影响其它分区。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-nt-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function statusPill(status) {
    var s = document.createElement('span');
    var cls = status === 'sent' ? 'pill-ok' : (status === 'failed' ? 'pill-bad' : 'pill-mute');
    s.className = 'pill ' + cls;
    s.textContent = status || '—';
    return s;
  }

  /** 服务端 HistoryEntry 未带 json tag ⇒ 键为首字母大写；两种形态都兼容。 */
  function field(e, name) {
    if (!e) { return ''; }
    var lower = name.charAt(0).toLowerCase() + name.slice(1);
    return e[lower] !== undefined ? e[lower] : (e[name] !== undefined ? e[name] : '');
  }

  function normalizeEntries(body) {
    var list = body && Array.isArray(body.entries) ? body.entries : [];
    return list.filter(Boolean).map(function (e) {
      return {
        ts: field(e, 'TS'), action: field(e, 'Action'), object: field(e, 'Object'),
        channel: field(e, 'Channel'), status: field(e, 'Status'), detail: field(e, 'Detail')
      };
    });
  }

  function render(entries) {
    var tbody = el('v2-nt-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    entries.forEach(function (e) {
      var tr = document.createElement('tr');
      var t = document.createElement('td'); t.className = 'num';
      t.textContent = e.ts ? fmt.formatTime(Date.parse(e.ts)) : '—';
      tr.appendChild(t);
      [e.action, e.object, e.channel].forEach(function (v) {
        var td = document.createElement('td');
        td.className = 'mono'; td.textContent = v || '—';
        tr.appendChild(td);
      });
      var st = document.createElement('td'); st.appendChild(statusPill(e.status));
      tr.appendChild(st);
      var dt = document.createElement('td'); dt.className = 'muted'; dt.textContent = e.detail || '';
      tr.appendChild(dt);
      tbody.appendChild(tr);
    });
    var empty = el('v2-nt-empty');
    if (empty) { empty.hidden = entries.length > 0; }
  }

  function load(keepBanner) {
    if (!keepBanner) { banner(''); }
    return api.json('GET', '/api/notify/history?limit=100')
      .then(function (body) { render(normalizeEntries(body)); })
      .catch(function (e) { render([]); banner('通知读取失败：' + (e.message || e), 'warn'); });
  }

  function testChannels() {
    banner('');
    api.json('POST', '/api/notify/test')
      .then(function (body) {
        var results = (body && body.results) || {};
        var names = Object.keys(results);
        if (!names.length) { banner('未注册任何通知渠道（服务端 notify.channels 为空）。'); return; }
        banner(names.map(function (n) { return n + ': ' + results[n]; }).join(' · '),
          names.some(function (n) { return String(results[n]).indexOf('ok') !== 0; }) ? 'warn' : '');
        load(true);
      })
      .catch(function (e) { banner('渠道自检失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var t = el('v2-nt-test'); if (t) { t.addEventListener('click', testChannels); }
    var r = el('v2-nt-refresh'); if (r) { r.addEventListener('click', load); }
    if (el('v2-nt-body')) { load(); }
  }

  if (typeof window !== 'undefined') { window.v2notify = { init: init, load: load }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { normalizeEntries: normalizeEntries, field: field };
  }
})();
