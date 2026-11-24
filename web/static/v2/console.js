// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 控制台（P3a）：概览（KPI / 存储水位 / 卷 / 最近审计）。
// 其余分区（云端任务/待处理/审计/配置/通知/巡检/接入）在 P3b 逐项接入。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var api = window.v2api, fmt = window.sharedFormat;
  if (!api || !fmt) { return; }

  function el(id) { return document.getElementById(id); }
  function isActive() { return (window.location.hash || '').indexOf('#/console') === 0; }

  function banner(kind, text) {
    var box = el('v2-cs-state');
    if (!box) { return; }
    if (!text) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text;
  }

  function kpi(label, value, sub) {
    var box = document.createElement('div');
    box.className = 'kpi';
    var k = document.createElement('div'); k.className = 'k'; k.textContent = label;
    var v = document.createElement('div'); v.className = 'v num'; v.textContent = value;
    var d = document.createElement('div'); d.className = 'd muted'; d.textContent = sub || '';
    box.appendChild(k); box.appendChild(v); box.appendChild(d);
    return box;
  }

  function pct(used, total) {
    if (!(total > 0)) { return null; }
    return Math.min(100, Math.round(used / total * 100));
  }

  function renderStats(s) {
    var box = el('v2-cs-kpis');
    if (!box) { return; }
    box.textContent = '';
    var files = s && s.disk_usage ? s.disk_usage.total_files : 0;
    var size = s && s.disk_usage ? s.disk_usage.total_size : 0;
    box.appendChild(kpi('文件数', String(files || 0), '用户桶（不含内部桶）'));
    box.appendChild(kpi('总大小', fmt.formatSize(size), '轻量遍历统计'));
    box.appendChild(kpi('活跃连接', String((s && s.active_connections) || 0), '当前 HTTP/隧道'));
    var rc = (s && s.request_counts) || {};
    box.appendChild(kpi('请求', String(rc.total || 0), '2xx ' + (rc['2xx'] || 0) + ' · 4xx ' + (rc['4xx'] || 0) + ' · 5xx ' + (rc['5xx'] || 0)));

    var usage = el('v2-cs-storage');
    if (!usage) { return; }
    usage.textContent = '';
    var used = Number(s && s.storage_usage) || 0;
    var max = Number(s && s.max_storage_bytes) || 0;
    var p = pct(used, max);
    var row = document.createElement('div');
    row.className = 'dr-row';
    var label = document.createElement('span');
    label.textContent = fmt.formatSize(used) + (max ? ' / ' + fmt.formatSize(max) : ' / 不限');
    var pctText = document.createElement('span');
    pctText.className = 'muted num';
    pctText.textContent = p == null ? '—' : (p + '%');
    row.appendChild(label); row.appendChild(pctText);
    usage.appendChild(row);
    var bar = document.createElement('div');
    bar.className = 'bar' + (p != null && p >= 90 ? ' bad' : p != null && p >= 70 ? ' warn' : '');
    var fill = document.createElement('i');
    fill.style.width = (p == null ? 0 : p) + '%';
    bar.appendChild(fill);
    usage.appendChild(bar);
    [['用户文件', s.storage_user_files], ['分块', s.storage_chunked], ['版本', s.storage_versions], ['云端', s.storage_cloud]]
      .forEach(function (kv) {
        var r = document.createElement('div');
        r.className = 'dr-row';
        r.style.fontSize = '12px';
        var k = document.createElement('span'); k.className = 'muted'; k.textContent = kv[0];
        var v = document.createElement('span'); v.className = 'num'; v.textContent = fmt.formatSize(kv[1]);
        r.appendChild(k); r.appendChild(v);
        usage.appendChild(r);
      });
  }

  function renderVolumes(body) {
    var tbody = el('v2-cs-volumes');
    if (!tbody) { return; }
    tbody.textContent = '';
    var list = (body && body.volumes) || [];
    list.forEach(function (v) {
      var tr = document.createElement('tr');
      var name = document.createElement('td'); name.className = 'mono'; name.textContent = v.name;
      var type = document.createElement('td'); type.textContent = v.type || '—';
      var mode = document.createElement('td'); mode.className = 'muted'; mode.textContent = v.mode || '—';
      var cap = document.createElement('td'); cap.className = 'r num'; cap.textContent = fmt.formatSize(v.capacity);
      var use = document.createElement('td'); use.className = 'r num';
      var p = pct(v.usage, v.capacity);
      use.textContent = fmt.formatSize(v.usage) + (p == null ? '' : ' · ' + p + '%');
      tr.appendChild(name); tr.appendChild(type); tr.appendChild(mode); tr.appendChild(cap); tr.appendChild(use);
      tbody.appendChild(tr);
    });
    var empty = el('v2-cs-volumes-empty');
    if (empty) { empty.hidden = list.length > 0; }
  }

  function renderAudit(body) {
    var tbody = el('v2-cs-audit');
    if (!tbody) { return; }
    tbody.textContent = '';
    var list = (body && (body.entries || body.items)) || [];
    list.slice(0, 8).forEach(function (e) {
      var tr = document.createElement('tr');
      var ts = document.createElement('td'); ts.className = 'num muted';
      ts.textContent = e.ts ? fmt.formatTime(Date.parse(e.ts)) : '—';
      var action = document.createElement('td'); action.className = 'mono'; action.textContent = e.action || e.type || '—';
      var actor = document.createElement('td'); actor.textContent = e.actor || '—';
      var obj = document.createElement('td'); obj.className = 'mono'; obj.style.fontSize = '11.5px'; obj.textContent = e.object || '—';
      var res = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + (e.result === 'success' ? 'pill-ok' : e.result === 'failed' ? 'pill-bad' : 'pill-mute');
      pill.textContent = e.result || '—';
      res.appendChild(pill);
      tr.appendChild(ts); tr.appendChild(action); tr.appendChild(actor); tr.appendChild(obj); tr.appendChild(res);
      tbody.appendChild(tr);
    });
    var empty = el('v2-cs-audit-empty');
    if (empty) { empty.hidden = list.length > 0; }
  }

  function load() {
    banner('', '');
    api.json('GET', '/api/stats')
      .then(function (s) { renderStats(s); })
      .catch(function (e) {
        if (String(e.message || '').indexOf('尚未完成') !== -1 || String(e.message || '').indexOf('503') !== -1) {
          banner('warn', '存储统计尚未完成首次扫描，稍后刷新。');
        } else {
          banner('warn', '统计加载失败：' + (e.message || e));
        }
      });

    api.json('GET', '/api/volumes')
      .then(function (body) { renderVolumes(body); })
      .catch(function (e) { banner('warn', '卷加载失败：' + (e.message || e)); });

    api.json('GET', '/api/audit?limit=8')
      .then(function (body) { renderAudit(body); })
      .catch(function () { var card = el('v2-cs-audit-card'); if (card) { card.hidden = true; } });
  }

  function init() {
    var refresh = el('v2-cs-refresh');
    if (refresh) { refresh.addEventListener('click', load); }
    window.addEventListener('hashchange', function () { if (isActive()) { load(); } });
    Array.prototype.forEach.call(document.querySelectorAll('[data-cs]'), function (b) {
      b.addEventListener('click', function () {
        if (b.disabled) { return; }
        Array.prototype.forEach.call(document.querySelectorAll('[data-cs]'), function (x) { x.removeAttribute('aria-current'); });
        b.setAttribute('aria-current', 'page');
      });
    });
    if (isActive()) { load(); }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
