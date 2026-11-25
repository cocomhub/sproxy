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

  var state = { tasks: [], groups: [], syncs: [], filter: '', timer: null, auto: true, loading: false };

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
    var rows = state.tasks.filter(matchFilter);
    window.v2cloudTable.render(body, rows, {
      onAction: function (kind, t) { taskAction(kind, t); },
      onPreview: function (t) {
        if (window.v2preview && window.v2preview.openCloudTask) {
          window.v2preview.openCloudTask(t.id, t.filename);
        }
      }
    });
    var empty = el('v2-tf-empty');
    if (empty) { empty.hidden = rows.length > 0; }
    var s = summarize(state.tasks);
    var set = function (id, v) { var n = el(id); if (n) { n.textContent = String(v); } };
    set('v2-tf-all', s.total); set('v2-tf-running', s.running);
    set('v2-tf-failed', s.failed); set('v2-tf-completed', s.completed);
  }

  function taskAction(kind, t) {
    window.v2cloudTable.perform(kind, t, api, function (ok, e) {
      if (ok) { if (window.v2toast) { window.v2toast('ok', '已' + (kind === 'delete' ? '删除' : kind === 'retry' ? '重试' : '取消') + '任务'); } load(); }
      else if (window.v2toast) { window.v2toast('bad', '操作失败：' + (e.message || e)); }
    });
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
      var prog = window.v2chain ? window.v2chain.groupProgress(g) : null;
      if (g.failed) {
        actTd.appendChild(actionBtn('重试失败项', function () {
          run(api.json('POST', '/api/cloud/tasks/retry', {
            body: new TextEncoder().encode(JSON.stringify({ group_id: g.id, force: true })),
            contentType: 'application/json'
          }), '已提交重试失败项');
        }));
      }
      if (g.status !== 'downloading' && g.status !== 'pending') {
        actTd.appendChild(actionBtn('打包已完成', function () { archiveGroup(g, false); }));
        // 「打包全部」始终可见：未全部完成时服务端 require_all 会 409 并列出未完成项
        // （让用户明确知道「全有」不满足，而不是按钮凭空消失）。
        if (prog && prog.total > 0) {
          actTd.appendChild(actionBtn('打包全部', function () { archiveGroup(g, true); }));
        }
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

  /** 组归档：requireAll=true 时服务端「全有或全无」（有未完成项 ⇒ 409 并列出）。 */
  function archiveGroup(g, requireAll) {
    var name = window.v2chain ? window.v2chain.defaultArchiveName() : 'archive.tar.gz';
    if (g.name) { name = String(g.name).replace(/[^\w.-]+/g, '-') + '.tar.gz'; }
    var payload = { archive_name: name };
    if (requireAll) { payload.require_all = true; }
    api.json('POST', '/api/cloud/groups/' + encodeURIComponent(g.id) + '/archive', {
      body: new TextEncoder().encode(JSON.stringify(payload)),
      contentType: 'application/json'
    }).then(function (res) {
      if (window.v2toast) { window.v2toast('ok', '已打包 ' + ((res && res.file) || name)); }
      loadGroups();
    }).catch(function (e) {
      if (window.v2toast) { window.v2toast('bad', '打包失败：' + (e.message || e)); }
    });
  }

  // chainRun 启动一个链式下载的收尾编排：轮询组到终态 → 全部完成则按 config 归档。
  var chainRuns = Object.create(null);

  function chainRun(cfg) {
    if (!cfg || !cfg.groupID) { return; }
    var gid = cfg.groupID;
    if (chainRuns[gid]) { return; }
    chainRuns[gid] = { cfg: cfg, timer: null };
    var tick = function () {
      if ((window.location.hash || '').indexOf('#/transfer') !== 0) { return; }
      api.json('GET', '/api/cloud/groups/' + encodeURIComponent(gid))
        .then(function (g) {
          var prog = window.v2chain ? window.v2chain.groupProgress(g) : { ready: false };
          if (prog.ready) {
            stopChain(gid);
            if (cfg.autoArchive) { archiveGroup(g, true); }
            if (window.v2toast) { window.v2toast('ok', '链式下载完成：全部 ' + prog.total + ' 项就绪'); }
          } else if (g && (g.status === 'failed' || g.status === 'cancelled')) {
            stopChain(gid);
            if (window.v2toast) {
              window.v2toast('warn', '链式下载未全部完成（失败 ' + (prog.failed || 0) + ' 项），请重试失败项或打包已完成');
            }
          }
          loadGroups();
        })
        .catch(function () { /* 网络抖动：下个周期再试 */ });
    };
    chainRuns[gid].timer = setInterval(tick, 3000);
    tick();
  }

  function stopChain(gid) {
    var r = chainRuns[gid];
    if (r && r.timer) { clearInterval(r.timer); }
    delete chainRuns[gid];
  }

  window.v2chainRun = chainRun;

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

  /** 载体文本：kind（+transport），终态再附实际载体计数。 */
  function carrierText(t) {
    var parts = [];
    if (t.kind) { parts.push(t.kind); }
    if (t.transport) { parts.push(t.transport); }
    if (t.carriers && typeof t.carriers === 'object') {
      var segs = Object.keys(t.carriers).map(function (k) { return k + '×' + t.carriers[k]; });
      if (segs.length) { parts.push(segs.join(' ')); }
    }
    return parts.length ? parts.join(' · ') : '—';
  }

  function syncStatusLabel(s) {
    var m = { pending: '排队中', running: '同步中', syncing: '同步中', completed: '已完成', failed: '失败', cancelled: '已取消' };
    return m[s] || s || '—';
  }

  function renderSyncs() {
    var body = el('v2-sync-body');
    if (!body) { return; }
    body.textContent = '';
    state.syncs.forEach(function (t) {
      var tr = document.createElement('tr');
      var idTd = document.createElement('td');
      idTd.className = 'mono';
      idTd.textContent = t.id;
      tr.appendChild(idTd);

      var dirTd = document.createElement('td');
      dirTd.textContent = t.direction || '—';
      tr.appendChild(dirTd);

      var pathTd = document.createElement('td');
      pathTd.className = 'mono';
      pathTd.style.fontSize = '11.5px';
      pathTd.textContent = (t.src || '') + ' → ' + (t.remote || '') + ':' + (t.dst || '');
      tr.appendChild(pathTd);

      var stTd = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + statusClass(t.status === 'running' ? 'downloading' : t.status);
      pill.textContent = syncStatusLabel(t.status);
      stTd.appendChild(pill);
      tr.appendChild(stTd);

      // 载体可见性（W1）：kind / transport / 终态 carriers 计数（服务端快照字段）。
      var carTd = document.createElement('td');
      carTd.className = 'muted';
      carTd.style.fontSize = '11.5px';
      carTd.textContent = carrierText(t);
      carTd.setAttribute('data-carrier', t.kind || '');
      tr.appendChild(carTd);

      var pgTd = document.createElement('td');
      pgTd.className = 'num muted';
      pgTd.style.fontSize = '11.5px';
      pgTd.textContent = (t.files_done || 0) + ' / ' + (t.files_total || 0) + ' 文件';
      tr.appendChild(pgTd);

      var actTd = document.createElement('td');
      actTd.className = 'r actions';
      if (t.status === 'running' || t.status === 'pending' || t.status === 'syncing') {
        actTd.appendChild(actionBtn('取消', function () {
          run(api.json('POST', '/api/sync/tasks/' + encodeURIComponent(t.id) + '/cancel', {}), '已取消同步');
        }));
      }
      actTd.appendChild(actionBtn('删除', function () {
        run(api.json('DELETE', '/api/sync/tasks/' + encodeURIComponent(t.id)), '已删除同步任务');
      }));
      tr.appendChild(actTd);
      body.appendChild(tr);
    });
    var empty = el('v2-sync-empty');
    if (empty) { empty.hidden = state.syncs.length > 0; }
  }

  function loadSyncs() {
    if (!el('v2-sync-body')) { return; }
    setSyncState('', '');
    api.json('GET', '/api/sync/tasks')
      .then(function (body) {
        state.syncs = (body && (body.tasks || body.items)) || [];
        renderSyncs();
      })
      .catch(function (e) { setSyncState('error', '同步任务加载失败：' + (e.message || e)); });
  }

  function setSyncState(kind, text) {
    var box = el('v2-sync-state');
    if (!box) { return; }
    if (!kind) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = text;
  }

  function openSyncDialog() { var d = el('v2-sync-backdrop'); if (d) { d.hidden = false; el('v2-sync-remote').focus(); } }
  function closeSyncDialog() { var d = el('v2-sync-backdrop'); if (d) { d.hidden = true; } }

  function submitSync() {
    var body = {
      direction: el('v2-sync-dir').value,
      remote: el('v2-sync-remote').value.trim(),
      src: el('v2-sync-src').value.trim(),
      dst: el('v2-sync-dst').value.trim(),
      recursive: el('v2-sync-recursive').checked,
      conflict_policy: el('v2-sync-conflict').value,
      delete_policy: el('v2-sync-delete').value
    };
    if (!body.remote) { if (window.v2toast) { window.v2toast('warn', '请填写远端节点'); } return; }
    api.json('POST', '/api/sync/tasks', {
      body: new TextEncoder().encode(JSON.stringify(body)),
      contentType: 'application/json'
    }).then(function () {
      closeSyncDialog();
      if (window.v2toast) { window.v2toast('ok', '已创建同步任务'); }
      loadSyncs();
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
    loadSyncs();
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
    // 页面卸载时清掉轮询定时器（chain 运行器 + 自动刷新），避免后台空转。
    window.addEventListener('beforeunload', function () {
      Object.keys(chainRuns).forEach(function (gid) { stopChain(gid); });
      if (state.timer) { clearInterval(state.timer); state.timer = null; }
    });
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
    var sr = el('v2-sync-refresh');
    if (sr) { sr.addEventListener('click', loadSyncs); }
    var sc = el('v2-sync-create');
    if (sc) { sc.addEventListener('click', openSyncDialog); }
    var sx = el('v2-sync-cancel');
    if (sx) { sx.addEventListener('click', closeSyncDialog); }
    var sok = el('v2-sync-ok');
    if (sok) { sok.addEventListener('click', submitSync); }
    var sb = el('v2-sync-backdrop');
    if (sb) { sb.addEventListener('click', function (e) { if (e.target === sb) { closeSyncDialog(); } }); }
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { closeSyncDialog(); } });

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
