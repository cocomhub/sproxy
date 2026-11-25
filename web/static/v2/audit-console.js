// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台审计分区：过滤（动作/主体/卷/起始时间/条数）+ 列表 + NDJSON 导出。
// 读面即 GET /api/audit（条目倒序）与 GET /api/audit/export（正序全量）。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  /** 由过滤状态构造 query（空值省略；时间转 RFC3339）。 */
  function buildQuery(state, timeKey) {
    var s = state || {};
    var parts = [];
    var add = function (k, v) {
      if (v === undefined || v === null) { return; }
      var t = String(v).trim();
      if (!t) { return; }
      parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(t));
    };
    add('action', s.action);
    add('actor', s.actor);
    add('volume', s.volume);
    add(timeKey || 'since', s.since);
    add('limit', s.limit);
    return parts.join('&');
  }

  function normalizeEvents(body) {
    if (!body || !Array.isArray(body.events)) { return []; }
    return body.events.filter(function (e) { return e && e.ts; });
  }

  function resultPill(result) {
    var span = document.createElement('span');
    var cls = result === 'success' ? 'pill-ok' : (result === 'denied' ? 'pill-warn' : 'pill-bad');
    span.className = 'pill ' + cls;
    span.textContent = result || '—';
    return span;
  }

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-au-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function render(events) {
    var tbody = el('v2-au-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    events.forEach(function (e) {
      var tr = document.createElement('tr');
      var cells = [
        fmt.formatTime(Date.parse(e.ts)),
        e.action || '—',
        e.actor || '—',
        e.volume || '—',
        (e.object_type || '') + (e.object ? ': ' + e.object : ''),
        e.detail || ''
      ];
      cells.forEach(function (text, i) {
        var td = document.createElement('td');
        td.textContent = text;
        if (i === 0 || i === 1) { td.className = 'mono'; }
        if (i === 5) { td.className = 'muted'; td.title = text; }
        tr.appendChild(td);
      });
      var st = document.createElement('td');
      st.appendChild(resultPill(e.result));
      tr.appendChild(st);
      tbody.appendChild(tr);
    });
    var empty = el('v2-au-empty');
    if (empty) { empty.hidden = events.length > 0; }
  }

  function isoOrEmpty(v) {
    if (!v) { return ''; }
    var d = new Date(v);
    return isNaN(d.getTime()) ? '' : d.toISOString();
  }

  function readState() {
    var since = (el('v2-au-since') || {}).value || '';
    return {
      action: (el('v2-au-action') || {}).value || '',
      actor: (el('v2-au-actor') || {}).value || '',
      volume: (el('v2-au-volume') || {}).value || '',
      // datetime-local 无时区 ⇒ 视为本地时间，转 RFC3339（带偏移）。
      since: isoOrEmpty(since),
      limit: (el('v2-au-limit') || {}).value || '100'
    };
  }

  var loading = false;

  function load() {
    if (loading) { return; }
    loading = true;
    banner('');
    var q = buildQuery(readState());
    api.json('GET', '/api/audit' + (q ? '?' + q : ''))
      .then(function (body) { render(normalizeEvents(body)); })
      .catch(function (e) {
        render([]);
        banner('审计读取失败：' + (e.message || e), 'warn');
      })
      .then(function () { loading = false; });
  }

  function exportNdjson() {
    var q = buildQuery(readState(), 'after_ts');
    banner('');
    api.request('GET', '/api/audit/export' + (q ? '?' + q : ''))
      .then(api.ensureOk)
      .then(function (resp) { return resp.blob(); })
      .then(function (blob) {
        var url = URL.createObjectURL(blob);
        var a = document.createElement('a');
        a.href = url;
        // 服务端 /api/audit/export 返回 JSON 数组（时间正序），扩展名保持一致。
        a.download = 'audit-' + new Date().toISOString().slice(0, 10) + '.json';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
        if (window.v2toast) { window.v2toast('ok', '已导出审计 JSON'); }
      })
      .catch(function (e) { banner('导出失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var apply = el('v2-au-apply'); if (apply) { apply.addEventListener('click', load); }
    var reset = el('v2-au-reset');
    if (reset) {
      reset.addEventListener('click', function () {
        ['v2-au-action', 'v2-au-actor', 'v2-au-volume', 'v2-au-since'].forEach(function (id) {
          var n = el(id); if (n) { n.value = ''; }
        });
        var lim = el('v2-au-limit'); if (lim) { lim.value = '100'; }
        load();
      });
    }
    var exp = el('v2-au-export'); if (exp) { exp.addEventListener('click', exportNdjson); }
    var tbody = el('v2-au-body');
    if (tbody) { load(); }
  }

  if (typeof window !== 'undefined') { window.v2audit = { init: init, load: load, buildQuery: buildQuery, normalizeEvents: normalizeEvents }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { buildQuery: buildQuery, normalizeEvents: normalizeEvents };
  }
})();
