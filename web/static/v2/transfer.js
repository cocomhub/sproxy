// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 传输页（P2a）：云端任务列表/筛选/操作/自动刷新。
// 展示逻辑复用 cloud-tasks.js 的 ctStatusLabel/ctProgressPercent/ctSummary 等纯函数。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var api = window.v2api, fmt = window.sharedFormat;
  var ct = window.ctStatusLabel ? window : null; // cloud-tasks.js 以全局函数暴露
  if (!api || !fmt) { return; }

  var state = { tasks: [], groups: [], filter: '', timer: null, auto: true, loading: false };

  function el(id) { return document.getElementById(id); }

  function statusClass(s) {
    if (s === 'completed') { return 'pill-ok'; }
    if (s === 'failed') { return 'pill-bad'; }
    if (s === 'cancelled') { return 'pill-mute'; }
    return 'pill-info';
  }
  function label(s) { return ct ? ct.ctStatusLabel(s) : (s || '—'); }
  function pct(t) {
    var p = ct ? ct.ctProgressPercent(t) : null;
    return p == null ? null : p;
  }
  function summarize(list) {
    if (ct && ct.ctSummarize) { return ct.ctSummarize(list); }
    var out = { total: 0, running: 0, failed: 0, completed: 0 };
    list.forEach(function (t) {
      out.total += 1;
      if (t.status === 'failed') { out.failed += 1; }
      else if (t.status === 'completed') { out.completed += 1; }
      else if (t.status !== 'cancelled') { out.running += 1; }
    });
    return out;
  }
  function matchFilter(t) {
    if (!state.filter) { return true; }
    if (state.filter === 'running') { return t.status === 'pending' || t.status === 'downloading'; }
    if (state.filter === 'failed') { return t.status === 'failed'; }
    if (state.filter === 'completed') { return t.status === 'completed'; }
    return true;
  }

  function setState(kind, text) {
    var box = el('v2-tf-state');
    if (!box) { return; }
    if (!kind) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = text;
  }

  function actionBtn(text, handler) {
    var b = document.createElement('button');
    b.type = 'button'; b.className = 'btn btn-sm btn-ghost'; b.textContent = text;
    b.addEventListener('click', handler);
    return b;
  }

  function run(promise, okMsg) {
    return promise.then(function () { if (window.v2toast) { window.v2toast('ok', okMsg); } load(); })
      .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '操作失败：' + (e.message || e)); } });
  }

  function render() {
    var body = el('v2-tf-body');
    if (!body) { return; }
    body.textContent = '';
    var rows = state.tasks.filter(matchFilter);
    rows.forEach(function (t) {
      var tr = document.createElement('tr');
      var nameTd = document.createElement('td');
      nameTd.className = 'mono';
      nameTd.textContent = t.filename || t.url || t.id;
      if (t.filename && t.url) { nameTd.title = t.url; }
      tr.appendChild(nameTd);

      var stTd = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + statusClass(t.status);
      pill.textContent = label(t.status);
      stTd.appendChild(pill);
      tr.appendChild(stTd);

      var pgTd = document.createElement('td');
      var p = pct(t);
      var bar = document.createElement('div'); bar.className = 'bar';
      var fill = document.createElement('i');
      fill.style.width = (p == null ? 0 : p) + '%';
      if (t.status === 'failed') { bar.className = 'bar bad'; }
      bar.appendChild(fill);
      var txt = document.createElement('div');
      txt.className = 'muted'; txt.style.fontSize = '11px';
      txt.textContent = p == null ? '—' : (p + '%');
      pgTd.appendChild(bar); pgTd.appendChild(txt);
      tr.appendChild(pgTd);

      var szTd = document.createElement('td');
      szTd.className = 'r num';
      szTd.textContent = fmt.formatSize(t.total_size);
      tr.appendChild(szTd);

      var upTd = document.createElement('td');
      upTd.className = 'num muted';
      upTd.textContent = t.updated_at ? fmt.formatTime(Date.parse(t.updated_at)) : '—';
      tr.appendChild(upTd);

      var actTd = document.createElement('td');
      actTd.className = 'r actions';
      if (t.status === 'pending' || t.status === 'downloading') {
        actTd.appendChild(actionBtn('取消', function () {
          run(api.json('POST', '/api/cloud/tasks/' + encodeURIComponent(t.id) + '/cancel', {}), '已取消任务');
        }));
      }
      if (t.status === 'failed') {
        actTd.appendChild(actionBtn('重试', function () {
          run(api.json('POST', '/api/cloud/tasks/retry', {
            body: new TextEncoder().encode(JSON.stringify(ct && ct.ctRetryPayload ? ct.ctRetryPayload(t.id, true) : { task_id: t.id, force: true })),
            contentType: 'application/json'
          }), '已提交重试');
        }));
      }
      if (t.status === 'completed' || t.status === 'cancelled' || t.status === 'failed') {
        actTd.appendChild(actionBtn('删除', function () {
          run(api.json('DELETE', '/api/cloud/tasks/' + encodeURIComponent(t.id)), '已删除任务');
        }));
      }
      tr.appendChild(actTd);
      body.appendChild(tr);
    });
    var empty = el('v2-tf-empty');
    if (empty) { empty.hidden = rows.length > 0; }
    var s = summarize(state.tasks);
    var set = function (id, v) { var n = el(id); if (n) { n.textContent = String(v); } };
    set('v2-tf-all', s.total); set('v2-tf-running', s.running);
    set('v2-tf-failed', s.failed); set('v2-tf-completed', s.completed);
  }

  function renderGroups() {
    var body = el('v2-grp-body');
    if (!body) { return; }
    body.textContent = '';
    state.groups.forEach(function (g) {
      var tr = document.createElement('tr');
      var nameTd = document.createElement('td');
      nameTd.textContent = g.name || g.id;
      nameTd.className = 'mono';
      tr.appendChild(nameTd);

      var stTd = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + statusClass(g.status);
      pill.textContent = label(g.status);
      stTd.appendChild(pill);
      tr.appendChild(stTd);

      var pgTd = document.createElement('td');
      var total = Number(g.total_tasks) || 0;
      var done = Number(g.completed) || 0;
      var p = total ? Math.round(done / total * 100) : 0;
      var bar = document.createElement('div'); bar.className = 'bar';
      var fill = document.createElement('i'); fill.style.width = p + '%';
      bar.appendChild(fill);
      var txt = document.createElement('div');
      txt.className = 'muted'; txt.style.fontSize = '11px';
      txt.textContent = done + ' / ' + total + ' · ' + p + '%';
      pgTd.appendChild(bar); pgTd.appendChild(txt);
      tr.appendChild(pgTd);

      var failTd = document.createElement('td');
      failTd.className = 'r num';
      failTd.textContent = String(g.failed || 0);
      tr.appendChild(failTd);

      var upTd = document.createElement('td');
      upTd.className = 'num muted';
      upTd.textContent = g.updated_at ? fmt.formatTime(Date.parse(g.updated_at)) : '—';
      tr.appendChild(upTd);

      var actTd = document.createElement('td');
      actTd.className = 'r actions';
      if (g.status === 'downloading' || g.status === 'pending') {
        actTd.appendChild(actionBtn('取消', function () {
          run(api.json('POST', '/api/cloud/groups/' + encodeURIComponent(g.id) + '/cancel', {}), '已取消组');
        }));
      }
      if (g.status === 'failed' || g.status === 'cancelled') {
        actTd.appendChild(actionBtn('整组重试', function () {
          run(api.json('POST', '/api/cloud/groups/' + encodeURIComponent(g.id) + '/resume', {
            body: new TextEncoder().encode(JSON.stringify({ force: true })),
            contentType: 'application/json'
          }), '已提交整组重试');
        }));
      }
      if (g.status !== 'downloading' && g.status !== 'pending') {
        actTd.appendChild(actionBtn('删除', function () {
          run(api.json('DELETE', '/api/cloud/groups/' + encodeURIComponent(g.id)), '已删除组');
        }));
      }
      tr.appendChild(actTd);
      body.appendChild(tr);
    });
    var empty = el('v2-grp-empty');
    if (empty) { empty.hidden = state.groups.length > 0; }
  }

  function loadGroups() {
    setGroupState('', '');
    api.json('GET', '/api/cloud/groups?limit=50')
      .then(function (body) {
        state.groups = (body && (body.groups || body.items)) || [];
        renderGroups();
      })
      .catch(function (e) { setGroupState('error', '任务组加载失败：' + (e.message || e)); });
  }

  function setGroupState(kind, text) {
    var box = el('v2-grp-state');
    if (!box) { return; }
    if (!kind) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = text;
  }

  function openGroupDialog() {
    var d = el('v2-grp-backdrop');
    if (d) { d.hidden = false; el('v2-grp-name').focus(); }
  }
  function closeGroupDialog() {
    var d = el('v2-grp-backdrop');
    if (d) { d.hidden = true; }
  }
  function submitGroup() {
    var name = el('v2-grp-name').value.trim();
    var urls = el('v2-grp-urls').value.split(String.fromCharCode(10)).map(function (x) { return x.trim(); }).filter(Boolean);
    if (!name) { if (window.v2toast) { window.v2toast('warn', '组名不能为空'); } return; }
    if (!urls.length) { if (window.v2toast) { window.v2toast('warn', '至少一个下载链接'); } return; }
    api.json('POST', '/api/cloud/groups', {
      body: new TextEncoder().encode(JSON.stringify({ name: name, urls: urls })),
      contentType: 'application/json'
    }).then(function () {
      closeGroupDialog();
      el('v2-grp-urls').value = '';
      if (window.v2toast) { window.v2toast('ok', '已创建任务组：' + name); }
      loadGroups();
    }).catch(function (e) {
      if (window.v2toast) { window.v2toast('bad', '创建失败：' + (e.message || e)); }
    });
  }

  function load() {
    if (state.loading) { return; }
    state.loading = true;
    setState('', '');
    api.json('GET', '/api/cloud/tasks?limit=100')
      .then(function (body) {
        state.tasks = (body && (body.tasks || body.items)) || [];
        render();
      })
      .catch(function (e) {
        setState('error', '云端任务加载失败：' + (e.message || e));
      })
      .then(function () { state.loading = false; });
    loadGroups();
  }

  function setAuto(on) {
    state.auto = on;
    var pill = el('v2-tf-auto');
    if (pill) {
      pill.className = on ? 'pill pill-ok' : 'pill pill-mute';
      pill.setAttribute('aria-pressed', String(on));
      pill.innerHTML = (on ? '<span class="dot"></span>' : '') + (on ? '自动刷新 5s' : '自动刷新已关闭');
    }
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    if (on) { state.timer = setInterval(function () { if (isActive()) { load(); } }, 5000); }
  }

  function isActive() { return (window.location.hash || '').indexOf('#/transfer') === 0; }

  function init() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-tf]'), function (c) {
      c.addEventListener('click', function () {
        Array.prototype.forEach.call(document.querySelectorAll('[data-tf]'), function (x) { x.setAttribute('aria-pressed', 'false'); });
        c.setAttribute('aria-pressed', 'true');
        state.filter = c.getAttribute('data-tf');
        render();
      });
    });
    var refresh = el('v2-tf-refresh');
    if (refresh) { refresh.addEventListener('click', function () { load(); loadGroups(); }); }
    var gr = el('v2-grp-refresh');
    if (gr) { gr.addEventListener('click', loadGroups); }
    var gc = el('v2-grp-create');
    if (gc) { gc.addEventListener('click', openGroupDialog); }
    var gx = el('v2-grp-cancel');
    if (gx) { gx.addEventListener('click', closeGroupDialog); }
    var gok = el('v2-grp-ok');
    if (gok) { gok.addEventListener('click', submitGroup); }
    var gb = el('v2-grp-backdrop');
    if (gb) { gb.addEventListener('click', function (e) { if (e.target === gb) { closeGroupDialog(); } }); }
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { closeGroupDialog(); } });
    var auto = el('v2-tf-auto');
    if (auto) {
      auto.addEventListener('click', function () { setAuto(!state.auto); });
    }
    window.addEventListener('hashchange', function () { if (isActive()) { load(); } });
    if (isActive()) { load(); }
    setAuto(true);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
