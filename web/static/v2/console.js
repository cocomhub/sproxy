// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 控制台：概览、云端任务、待处理事项、审计、配置、通知、巡检、接入。
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

  var pane = 'overview';

  function setPane(name) {
    pane = name;
    Array.prototype.forEach.call(document.querySelectorAll('[data-cspane]'), function (p) {
      p.hidden = p.getAttribute('data-cspane') !== name;
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-cs]'), function (b) {
      if (b.getAttribute('data-cs') === name) { b.setAttribute('aria-current', 'page'); }
      else { b.removeAttribute('aria-current'); }
    });
    if (name === 'tasks') { loadTasks(); }
    if (name === 'issues') { loadIssues(); }
    if (name === 'audit' && window.v2audit) { window.v2audit.init(); }
    if (name === 'config' && window.v2config) { window.v2config.init(); }
    if (name === 'notify' && window.v2notify) { window.v2notify.init(); }
    if (name === 'verify' && window.v2verify) { window.v2verify.init(); }
    if (name === 'access' && window.v2access) { window.v2access.init(); }
  }

  function loadTasks() {
    var tbody = el('v2-cs-tasks');
    if (!tbody) { return; }
    api.json('GET', '/api/cloud/tasks?limit=100')
      .then(function (body) {
        var list = (body && (body.tasks || body.items)) || [];
        window.v2cloudTable.render(tbody, list, {
          onAction: function (kind, t) {
            window.v2cloudTable.perform(kind, t, api, function (ok, e) {
              if (ok) { if (window.v2toast) { window.v2toast('ok', '已处理任务'); } loadTasks(); }
              else if (window.v2toast) { window.v2toast('bad', '操作失败：' + (e.message || e)); }
            });
          }
        });
        var empty = el('v2-cs-tasks-empty');
        if (empty) { empty.hidden = list.length > 0; }
      })
      .catch(function (e) { banner('warn', '云端任务加载失败：' + (e.message || e)); });
  }

  function issueStatusClass(st) {
    if (st === 'resolved') { return 'pill-ok'; }
    if (st === 'ignored') { return 'pill-mute'; }
    return 'pill-warn';
  }

  function renderIssues(items) {
    var tbody = el('v2-cs-issues');
    if (!tbody) { return; }
    tbody.textContent = '';
    items.forEach(function (it) {
      var tr = document.createElement('tr');
      var kind = document.createElement('td');
      var kp = document.createElement('span');
      kp.className = 'pill pill-bad';
      kp.textContent = it.kind || '—';
      kind.appendChild(kp);
      tr.appendChild(kind);

      var vol = document.createElement('td');
      vol.textContent = it.volume || '—';
      tr.appendChild(vol);

      var path = document.createElement('td');
      var p1 = document.createElement('div'); p1.className = 'mono'; p1.textContent = it.path || '—';
      var p2 = document.createElement('div'); p2.className = 'muted'; p2.style.fontSize = '11px';
      p2.textContent = it.detail || '';
      path.appendChild(p1); path.appendChild(p2);
      tr.appendChild(path);

      var att = document.createElement('td'); att.className = 'r num'; att.textContent = String(it.attempts || 0);
      tr.appendChild(att);

      var last = document.createElement('td'); last.className = 'num muted';
      last.textContent = it.last_seen ? fmt.formatTime(Date.parse(it.last_seen)) : '—';
      tr.appendChild(last);

      var st = document.createElement('td');
      var sp = document.createElement('span');
      sp.className = 'pill ' + issueStatusClass(it.status);
      sp.textContent = it.status || '—';
      st.appendChild(sp);
      tr.appendChild(st);

      var act = document.createElement('td');
      act.className = 'r actions';
      if (it.status !== 'resolved' && it.status !== 'ignored') {
        var btn = document.createElement('button');
        btn.type = 'button'; btn.className = 'btn btn-sm btn-ghost'; btn.textContent = '解决';
        btn.addEventListener('click', function () { askResolve(it, act); });
        act.appendChild(btn);
      }
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var empty = el('v2-cs-issues-empty');
    if (empty) { empty.hidden = items.length > 0; }
  }

  function askResolve(it, cell) {
    cell.textContent = '';
    var input = document.createElement('input');
    input.type = 'text';
    input.placeholder = '处置说明（必填）';
    input.style.cssText = 'height:26px;padding:0 6px;border:1px solid var(--border-strong);border-radius:6px;background:var(--surface);color:var(--text);font-size:12px;width:150px';
    var ok = document.createElement('button');
    ok.type = 'button'; ok.className = 'btn btn-sm'; ok.textContent = '确认';
    var cancel = document.createElement('button');
    cancel.type = 'button'; cancel.className = 'btn btn-sm btn-ghost'; cancel.textContent = '取消';
    cancel.addEventListener('click', function () { loadIssues(); });
    ok.addEventListener('click', function () {
      var note = input.value.trim();
      if (!note) { if (window.v2toast) { window.v2toast('warn', '请填写处置说明'); } return; }
      api.json('POST', '/api/issues/resolve?id=' + encodeURIComponent(it.id) + '&note=' + encodeURIComponent(note), {})
        .then(function () { if (window.v2toast) { window.v2toast('ok', '已标记解决'); } loadIssues(); })
        .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '操作失败：' + (e.message || e)); } });
    });
    cell.appendChild(input); cell.appendChild(ok); cell.appendChild(cancel);
    input.focus();
  }

  function loadIssues() {
    var tbody = el('v2-cs-issues');
    if (!tbody) { return; }
    api.json('GET', '/api/issues?limit=100')
      .then(function (body) { renderIssues((body && body.items) || []); })
      .catch(function (e) { banner('warn', '待处理事项加载失败：' + (e.message || e)); });
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
        setPane(b.getAttribute('data-cs'));
      });
    });
    var tr = el('v2-cs-tasks-refresh'); if (tr) { tr.addEventListener('click', loadTasks); }
    var ir = el('v2-cs-issues-refresh'); if (ir) { ir.addEventListener('click', loadIssues); }
    if (isActive()) { load(); }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
