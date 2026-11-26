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

  /** 解析 #/console/<section>?q=&status=&vol=&sort=&order=&page=（未知 section 回落概览）。 */
  function parseConsoleHash() {
    var raw = String(window.location.hash || '').replace(/^#\/?/, '');
    var parts = raw.split('?');
    var seg = parts[0].split('/');
    if (seg[0] !== 'console') { return null; }
    var section = seg[1] || 'overview';
    if (CS_ORDER.indexOf(section) === -1) { section = 'overview'; }
    var params = {};
    (parts[1] || '').split('&').forEach(function (kv) {
      if (!kv) { return; }
      var i = kv.indexOf('=');
      var k = i < 0 ? kv : kv.slice(0, i);
      var v = i < 0 ? '' : kv.slice(i + 1);
      try { params[decodeURIComponent(k)] = decodeURIComponent(v); } catch (e) { /* 忽略坏参数 */ }
    });
    return { section: section, params: params };
  }

  function taskParams() {
    var q = [];
    if (tasks.q) { q.push('q=' + encodeURIComponent(tasks.q)); }
    if (tasks.status) { q.push('status=' + encodeURIComponent(tasks.status)); }
    if (tasks.vol) { q.push('vol=' + encodeURIComponent(tasks.vol)); }
    if (tasks.size) { q.push('size=' + encodeURIComponent(tasks.size)); }
    if (tasks.sort !== 'updated') { q.push('sort=' + encodeURIComponent(tasks.sort)); }
    if (tasks.order !== 'desc') { q.push('order=' + encodeURIComponent(tasks.order)); }
    if (tasks.page > 1) { q.push('page=' + tasks.page); }
    return q.length ? '?' + q.join('&') : '';
  }

  function syncTaskHash() {
    if (pane !== 'tasks') { return; }
    try { history.replaceState(null, '', '#/console/tasks' + taskParams()); } catch (e) { /* file:// */ }
  }

  function applyTaskParams(params) {
    var p = params || {};
    tasks.q = p.q || '';
    tasks.status = p.status || '';
    tasks.vol = p.vol || '';
    tasks.size = SIZE_RANGES[p.size] || p.size === 'unknown' ? p.size : '';
    tasks.sort = p.sort || 'updated';
    tasks.order = p.order === 'asc' ? 'asc' : 'desc';
    tasks.page = Math.max(1, Number(p.page) || 1);
    var q = el('v2-cs-tasks-q'); if (q) { q.value = tasks.q; }
    var st = el('v2-cs-tasks-status'); if (st) { st.value = tasks.status; }
    var sz = el('v2-cs-tasks-size'); if (sz) { sz.value = tasks.size; }
    var szSel = el('v2-cs-tasks-size');
    if (szSel) { szSel.addEventListener('change', function () { tasks.size = szSel.value; tasks.page = 1; renderTaskRows(); syncTaskHash(); }); }
    var so = el('v2-cs-tasks-sort'); if (so) { so.value = tasks.sort; }
    var od = el('v2-cs-tasks-order'); if (od) { od.value = tasks.order; }
  }

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
    var trend = kpi('近 1h 失败', '—', '云任务失败数');
    trend.id = 'v2-cs-trend';
    box.appendChild(trend);

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

  function setPane(name, params, fromHash) {
    pane = name;
    if (name === 'tasks') { applyTaskParams(params); }
    if (!fromHash) {
      var h = '#/console/' + name + (name === 'tasks' ? taskParams() : '');
      if (name === 'overview') { h = '#/console'; }
      try { history.replaceState(null, '', h); } catch (e) { /* file:// */ }
    }
    Array.prototype.forEach.call(document.querySelectorAll('[data-cspane]'), function (p) {
      p.hidden = p.getAttribute('data-cspane') !== name;
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-cs]'), function (b) {
      if (b.getAttribute('data-cs') === name) { b.setAttribute('aria-current', 'page'); }
      else { b.removeAttribute('aria-current'); }
    });
    if (name === 'overview') { load(); }
    if (name === 'tasks') { loadTasks(); }
    if (name === 'issues') { loadIssues(); }
    if (name === 'audit' && window.v2audit) { window.v2audit.init(); }
    if (name === 'config' && window.v2config) { window.v2config.init(); }
    if (name === 'notify' && window.v2notify) { window.v2notify.init(); }
    if (name === 'verify' && window.v2verify) { window.v2verify.init(); }
    if (name === 'access' && window.v2access) { window.v2access.init(); }
    if (name === 'volumes' && window.v2volumes) { window.v2volumes.init(); }
    if (name === 'shares' && window.v2shares) { window.v2shares.init(); }
    if (name === 'mesh' && window.v2mesh) { window.v2mesh.initMesh(); }
    if (name === 'hub' && window.v2mesh) { window.v2mesh.initHub(); }
  }

  var tasks = { all: [], selected: Object.create(null), q: '', status: '', vol: '', size: '', sort: 'updated', order: 'desc', page: 1, pageSize: 50 };
  var SIZE_RANGES = {
    'lt100m': [0, 100 * 1024 * 1024],
    '100m-1g': [100 * 1024 * 1024, 1024 * 1024 * 1024],
    '1g-10g': [1024 * 1024 * 1024, 10 * 1024 * 1024 * 1024],
    'gt10g': [10 * 1024 * 1024 * 1024, Number.MAX_SAFE_INTEGER]
  };

  /** 体积区间判定（unknown = total_size <= 0，服务端以 -1 表示未知）。 */
  function sizeMatches(t) {
    if (!tasks.size) { return true; }
    var size = Number(t.total_size) || 0;
    if (tasks.size === 'unknown') { return size <= 0; }
    var r = SIZE_RANGES[tasks.size];
    if (!r) { return true; }
    return size >= r[0] && size < r[1];
  }
  var CS_ORDER = ['overview', 'tasks', 'issues', 'audit', 'config', 'notify', 'verify', 'access', 'volumes', 'mesh', 'hub', 'shares'];

  function taskVolume(t) { return (t.transfer && t.transfer.volume) || ''; }

  function taskMatches(t) {
    if (tasks.status && t.status !== tasks.status) { return false; }
    if (tasks.vol) {
      var v = taskVolume(t) || '__none__';
      if (v !== tasks.vol) { return false; }
    }
    if (!sizeMatches(t)) { return false; }
    if (!tasks.q) { return true; }
    var q = tasks.q.toLowerCase();
    return ((t.filename || '') + ' ' + (t.id || '') + ' ' + (t.url || '')).toLowerCase().indexOf(q) !== -1;
  }

  function taskSortValue(t, key) {
    if (key === 'size') { return Number(t.total_size) || 0; }
    if (key === 'name') { return (t.filename || '').toLowerCase(); }
    return Date.parse(t.updated_at || t.created_at || '') || 0;
  }

  function filteredTasks() {
    var list = tasks.all.filter(taskMatches);
    var dir = tasks.order === 'asc' ? 1 : -1;
    if (tasks.sort === 'name') {
      list.sort(function (a, b) { return String(taskSortValue(a, 'name')).localeCompare(String(taskSortValue(b, 'name'))) * dir; });
    } else {
      list.sort(function (a, b) { return (taskSortValue(a, tasks.sort) - taskSortValue(b, tasks.sort)) * dir; });
    }
    return list;
  }

  function visibleTasks() {
    var list = filteredTasks();
    var start = (tasks.page - 1) * tasks.pageSize;
    return list.slice(start, start + tasks.pageSize);
  }

  function renderTaskGroups() {
    var box = el('v2-cs-tasks-groups');
    if (!box) { return; }
    box.textContent = '';
    var counts = { '': tasks.all.length, '__none__': 0 };
    tasks.all.forEach(function (t) {
      var v = taskVolume(t);
      if (!v) { counts.__none__ += 1; return; }
      counts[v] = (counts[v] || 0) + 1;
    });
    var add = function (value, label, n) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'chip';
      b.setAttribute('data-vol', value);
      b.setAttribute('aria-pressed', tasks.vol === value ? 'true' : 'false');
      b.textContent = label + ' ' + n;
      b.addEventListener('click', function () {
        tasks.vol = value; tasks.page = 1;
        renderTaskGroups(); renderTaskRows(); syncTaskHash();
      });
      box.appendChild(b);
    };
    add('', '全部卷', counts[''] || 0);
    Object.keys(counts).forEach(function (v) {
      if (v === '') { return; }
      add(v, v === '__none__' ? '未指定卷' : v, counts[v]);
    });
  }

  function renderPager(filteredTotal) {
    var label = el('v2-cs-tasks-page');
    var prev = el('v2-cs-tasks-prev');
    var next = el('v2-cs-tasks-next');
    var pages = Math.max(1, Math.ceil(filteredTotal / tasks.pageSize));
    if (tasks.page > pages) { tasks.page = pages; }
    if (label) { label.textContent = '第 ' + tasks.page + ' / ' + pages + ' 页 · ' + filteredTotal + ' 项'; }
    if (prev) { prev.disabled = tasks.page <= 1; }
    if (next) { next.disabled = tasks.page >= pages; }
  }

  function selectedTaskList() {
    return tasks.all.filter(function (t) { return tasks.selected[t.id]; });
  }

  function syncTaskActions() {
    var sel = selectedTaskList();
    var canRetry = sel.some(function (t) { return t.status === 'failed' || t.status === 'cancelled'; });
    var canCancel = sel.some(function (t) { return t.status === 'pending' || t.status === 'downloading'; });
    var r = el('v2-cs-tasks-retry'); if (r) { r.disabled = !canRetry; }
    var c = el('v2-cs-tasks-cancel'); if (c) { c.disabled = !canCancel; }
  }

  function renderTaskRows() {
    var tbody = el('v2-cs-tasks');
    if (!tbody) { return; }
    renderTaskGroups();
    var filtered = filteredTasks();
    renderPager(filtered.length);
    var list = visibleTasks();
    tasks.selected = tasks.selected || Object.create(null);
    window.v2cloudTable.render(tbody, list, {
      selectable: true,
      isSelected: function (t) { return !!tasks.selected[t.id]; },
      onToggle: function (t, on) { if (on) { tasks.selected[t.id] = true; } else { delete tasks.selected[t.id]; } syncTaskActions(); },
      onDetail: showTaskDetail,
      onPreview: function (t) {
        if (window.v2preview && window.v2preview.openCloudTask) {
          window.v2preview.openCloudTask(t.id, t.filename);
        }
      },
      onAction: function (kind, t) {
        window.v2cloudTable.perform(kind, t, api, function (ok, e) {
          if (ok) { if (window.v2toast) { window.v2toast('ok', '已处理任务'); } loadTasks(); }
          else if (window.v2toast) { window.v2toast('bad', '操作失败：' + (e.message || e)); }
        });
      }
    });
    var empty = el('v2-cs-tasks-empty');
    if (empty) { empty.hidden = list.length > 0; }
    syncTaskActions();
  }

  function loadTasks() {
    if (!el('v2-cs-tasks')) { return Promise.resolve(); }
    return api.json('GET', '/api/cloud/tasks?limit=200')
      .then(function (body) {
        tasks.all = (body && (body.tasks || body.items)) || [];
        tasks.selected = Object.create(null);
        renderTaskRows();
      })
      .catch(function (e) { banner('warn', '云端任务加载失败：' + (e.message || e)); });
  }

  function showTaskDetail(t) {
    var box = el('v2-cs-tasks-detail');
    var pre = el('v2-cs-tasks-detail-json');
    if (!box || !pre) { return; }
    api.json('GET', '/api/cloud/tasks/' + encodeURIComponent(t.id))
      .then(function (resp) {
        pre.textContent = JSON.stringify(resp, null, 2);
        box.hidden = false;
      })
      .catch(function (e) { pre.textContent = '读取失败：' + (e.message || e); box.hidden = false; });
  }

  /**
   * 批量执行任务动作。
   *
   * **走服务端批量端点**（`POST /api/cloud/tasks/actions/batch`，上限 50）：此前是前端串行
   * N 次单条请求 —— N 个任务 = N 次往返，且每条的可用性判据由前端猜。
   * 服务端批量端点逐项串行执行并回**逐项结果**，前端只负责分组与展示。
   *
   * 分组规则：`cancel` 走单条端点（批量端点只接受 6 种 plan 动作）；其余动作按
   * **服务端 plan 里该任务实际可用的动作**分组（同组一次批量请求）。
   *
   * @returns {Promise<number>} 成功条数
   */
  function batchTasks(kind) {
    var target = selectedTaskList().filter(function (t) {
      return kind === 'retry' ? (t.status === 'failed' || t.status === 'cancelled') : (t.status === 'pending' || t.status === 'downloading');
    });
    if (!target.length) { return Promise.resolve(0); }
    recordOp('批量' + (kind === 'retry' ? '重试' : '取消') + ' ' + target.length + ' 项（提交中）');

    if (kind === 'cancel') {
      // 批量端点不支持 cancel ⇒ 串行单条（保持既有语义，改由服务端校验状态）。
      var chain = Promise.resolve();
      var doneCancel = 0;
      target.forEach(function (t) {
        chain = chain.then(function () {
          return window.v2cloudTable.perform('cancel', t, api, function (ok) { if (ok) { doneCancel += 1; } });
        });
      });
      return chain.then(function () {
        finishBatch('批量取消', doneCancel, target.length, []);
        return doneCancel;
      });
    }

    // 重试：按**服务端 plan 给出的动作**分组（没有 plan 时回落 retry_download）。
    var groups = Object.create(null);
    target.forEach(function (t) {
      var act = pickRetryAction(t);
      (groups[act] = groups[act] || []).push(t.id);
    });
    var acts = Object.keys(groups);
    var chain2 = Promise.resolve({ ok: 0, total: 0, fails: [] });
    acts.forEach(function (act) {
      chain2 = chain2.then(function (acc) {
        var ids = groups[act].slice(0, 50); // 与端点上限一致；超出部分提示分次
        return api.json('POST', '/api/cloud/tasks/actions/batch', {
          body: new TextEncoder().encode(JSON.stringify({ action: act, ids: ids })),
          contentType: 'application/json'
        }).then(function (res) {
          var results = (res && res.results) || [];
          results.forEach(function (r) {
            acc.total += 1;
            if (r && r.ok) { acc.ok += 1; } else { acc.fails.push(r && r.error ? r.error : '未知错误'); }
          });
          if (groups[act].length > ids.length) {
            acc.fails.push('另有 ' + (groups[act].length - ids.length) + ' 项超出单次上限 50，请分次执行');
          }
          return acc;
        }, function (e) {
          acc.total += ids.length;
          acc.fails.push(act + '：' + (e.message || e));
          return acc;
        });
      });
    });
    return chain2.then(function (acc) {
      finishBatch('批量重试', acc.ok, acc.total, acc.fails);
      return acc.ok;
    });
  }

  /** 取该任务在服务端 plan 里的**重试动作**（无 plan / 无重试项时回落 retry_download）。 */
  function pickRetryAction(t) {
    if (Array.isArray(t && t.actions)) {
      for (var i = 0; i < t.actions.length; i += 1) {
        var a = t.actions[i];
        if (!a || a.disabled || !a.action) { continue; }
        if (a.action === 'retry_download' || a.action === 'retry_transfer' ||
            a.action === 'retry_transfer_overwrite' || a.action === 'rotate_source') {
          if (a.action === 'retry_transfer_overwrite') { continue; } // 破坏性动作不进批量
          return a.action;
        }
      }
    }
    return 'retry_download';
  }

  /** 批量结束的统一收尾：toast（含首个失败原因）+ 最近操作 + 刷新。 */
  function finishBatch(label, ok, total, fails) {
    // 文案保持既有「已处理 x/y 个任务」口径（e2e 与用户预期都按它断言），失败时补首个原因。
    var msg = '已处理 ' + ok + '/' + total + ' 个任务';
    if (window.v2toast) {
      window.v2toast(ok === total && !fails.length ? 'ok' : 'bad',
        label + '：' + msg + (fails.length ? '（' + fails[0] + '）' : ''));
    }
    recordOp(label + ' ' + ok + '/' + total + (fails.length ? ' · 失败 ' + fails.length : ''));
    loadTasks();
  }

  var RECENT_KEY = 'sproxy_v2_task_ops';

  function loadRecentOps() {
    try { return JSON.parse(sessionStorage.getItem(RECENT_KEY) || '[]') || []; } catch (e) { return []; }
  }

  function recordOp(text) {
    var list = loadRecentOps();
    list.unshift({ ts: Date.now(), text: text });
    list = list.slice(0, 5);
    try { sessionStorage.setItem(RECENT_KEY, JSON.stringify(list)); } catch (e) { /* 忽略 */ }
    renderRecentOps();
  }

  function renderRecentOps() {
    var box = el('v2-cs-tasks-recent');
    if (!box) { return; }
    var list = loadRecentOps();
    if (!list.length) { box.hidden = true; return; }
    box.hidden = false;
    box.textContent = '最近操作：' + list.map(function (it) {
      var d = new Date(it.ts);
      return d.toLocaleTimeString() + ' ' + it.text;
    }).join(' · ');
  }

  function importTasks(file) {
    if (!file) { return; }
    var reader = new FileReader();
    reader.onload = function () {
      var rows = window.v2cloudOps ? window.v2cloudOps.parseTSV(reader.result) : [];
      var targets = rows.filter(function (r) { return r.id || r.url; });
      if (!targets.length) { banner('warn', 'TSV 中没有可导入的 id/url 行'); return; }
      var done = 0;
      var chain = Promise.resolve();
      targets.forEach(function (row) {
        chain = chain.then(function () {
          var payload = row.id ? { task_id: row.id, force: true } : { url: row.url, force: true };
          return api.json('POST', '/api/cloud/tasks/retry', {
            body: new TextEncoder().encode(JSON.stringify(payload)),
            contentType: 'application/json'
          }).then(function () { done += 1; }).catch(function () { /* 单条失败继续 */ });
        });
      });
      chain.then(function () {
        if (window.v2toast) { window.v2toast(done ? 'ok' : 'bad', 'TSV 导入重试 ' + done + '/' + targets.length); }
        recordOp('导入 TSV · 重试 ' + done + '/' + targets.length);
        loadTasks();
      });
    };
    reader.readAsText(file);
  }

  function exportTasks() {
    var list = selectedTaskList();
    if (!list.length) { list = visibleTasks(); }
    var head = ['id', 'status', 'filename', 'total_size', 'url', 'updated_at'];
    var lines = [head.join('	')];
    list.forEach(function (t) {
      lines.push([t.id || '', t.status || '', t.filename || '', String(t.total_size || 0), t.url || '', t.updated_at || ''].join('	'));
    });
    var nl = String.fromCharCode(10);
    var blob = new Blob([lines.join(nl) + nl], { type: 'text/tab-separated-values' });
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'cloud-tasks-' + new Date().toISOString().slice(0, 10) + '.tsv';
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
  }

  function issueStatusClass(st) {
    if (st === 'resolved') { return 'pill-ok'; }
    if (st === 'ignored') { return 'pill-mute'; }
    return 'pill-warn';
  }

  var issues = { all: [], q: '', status: '' };

  function issueMatches(it) {
    if (issues.status && it.status !== issues.status) { return false; }
    if (!issues.q) { return true; }
    var q = issues.q.toLowerCase();
    return ((it.path || '') + ' ' + (it.detail || '') + ' ' + (it.kind || '') + ' ' + (it.volume || ''))
      .toLowerCase().indexOf(q) !== -1;
  }

  function visibleIssues() { return issues.all.filter(issueMatches); }

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
      // 云端下载失败：行内三动作（重试下载 / 打包已完成 / 预览已完成），见方案 §4。
      if (it.kind === 'cloud_download_failed' && it.status !== 'resolved') {
        act.appendChild(issueBtn('重试下载', function () { retryCloudIssue(it); }));
        act.appendChild(issueBtn('打包已完成', function () { archiveCloudIssue(it); }));
        act.appendChild(issueBtn('预览已完成', function () { previewCloudIssue(it); }));
      }
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

  function issueBtn(text, handler) {
    var b = document.createElement('button');
    b.type = 'button'; b.className = 'btn btn-sm btn-ghost'; b.textContent = text;
    b.addEventListener('click', handler);
    return b;
  }

  /** 从事项 detail 里取 task=<id>（服务端登记格式：`task=xxx 类别=…`）。 */
  function issueTaskID(it) {
    var m = /task=([^\s；]+)/.exec(String(it.detail || ''));
    return m ? m[1] : '';
  }

  /** 重试下载：按转存目标（volume+path）或 URL 触发，与 CLI 同一内核。 */
  function retryCloudIssue(it) {
    var payload;
    if (it.volume && it.volume !== 'cloud' && it.path) {
      payload = { transfer_volume: it.volume, transfer_path: it.path, force: true };
    } else if (it.path) {
      payload = { url: it.path, force: true };
    } else {
      if (window.v2toast) { window.v2toast('warn', '该事项缺少可重试的来源'); }
      return;
    }
    api.json('POST', '/api/cloud/tasks/retry', {
      body: new TextEncoder().encode(JSON.stringify(payload)), contentType: 'application/json'
    }).then(function () {
      if (window.v2toast) { window.v2toast('ok', '已提交重试'); }
      loadIssues();
    }).catch(function (e) { if (window.v2toast) { window.v2toast('bad', '重试失败：' + (e.message || e)); } });
  }

  /** 打包已完成：对该任务所在组归档（不带 require_all，允许部分交付）；无组则对单任务归档。 */
  function archiveCloudIssue(it) {
    var taskID = issueTaskID(it);
    if (!taskID) { if (window.v2toast) { window.v2toast('warn', '事项未记录任务 ID，无法打包'); } return; }
    api.json('GET', '/api/cloud/tasks/' + encodeURIComponent(taskID))
      .then(function (task) {
        var gid = (task && task.group_id) || '';
        if (gid) {
          return api.json('POST', '/api/cloud/groups/' + encodeURIComponent(gid) + '/archive', {
            body: new TextEncoder().encode(JSON.stringify({})), contentType: 'application/json'
          });
        }
        // 无组：回落单任务归档（服务端已有 /tasks/{id}/archive）。
        return api.json('POST', '/api/cloud/tasks/' + encodeURIComponent(taskID) + '/archive', {
          body: new TextEncoder().encode(JSON.stringify({})), contentType: 'application/json'
        });
      })
      .then(function (res) {
        if (window.v2toast) { window.v2toast('ok', '已打包 ' + ((res && res.file) || '')); }
      })
      .catch(function (e) { if (window.v2toast) { window.v2toast('bad', '打包失败：' + (e.message || e)); } });
  }

  /** 预览已完成：任务 completed 时直接打开预览浮层（cloud 桶文件，kind=cloud_task）。 */
  function previewCloudIssue(it) {
    var taskID = issueTaskID(it);
    if (!taskID) { if (window.v2toast) { window.v2toast('warn', '事项未记录任务 ID，无法预览'); } return; }
    api.json('GET', '/api/cloud/tasks/' + encodeURIComponent(taskID))
      .then(function (task) {
        if (!task || task.status !== 'completed') { throw new Error('任务尚未完成，暂无可预览文件'); }
        if (!window.v2preview || !window.v2preview.openCloudTask) { throw new Error('预览模块不可用'); }
        window.v2preview.openCloudTask(task.id, task.filename);
      })
      .catch(function (e) { if (window.v2toast) { window.v2toast('warn', e.message || String(e)); } });
  }

  function exportIssues() {
    var list = visibleIssues();
    if (!list.length) { if (window.v2toast) { window.v2toast('warn', '没有可导出的条目'); } return; }
    var head = ['id', 'kind', 'volume', 'path', 'status', 'attempts', 'last_seen', 'detail'];
    var rows = [head.join(',')];
    var esc = function (v) { return '"' + String(v == null ? '' : v).replace(/"/g, '""') + '"'; };
    list.forEach(function (it) {
      rows.push([it.id, it.kind, it.volume, it.path, it.status, it.attempts, it.last_seen, it.detail].map(esc).join(','));
    });
    var blob = new Blob([rows.join(String.fromCharCode(10)) + String.fromCharCode(10)], { type: 'text/csv' });
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'issues-' + new Date().toISOString().slice(0, 10) + '.csv';
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
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
      .then(function (body) {
        issues.all = (body && body.items) || [];
        renderIssues(visibleIssues());
      })
      .catch(function (e) { banner('warn', '待处理事项加载失败：' + (e.message || e)); });
  }

  /** 近 1 小时云任务失败数（真实 /api/cloud/tasks?status=failed，按 updated_at 过滤）。 */
  function loadTrend() {
    var box = el('v2-cs-trend');
    if (!box) { return; }
    var v = box.querySelector('.v');
    api.json('GET', '/api/cloud/tasks?status=failed&limit=200')
      .then(function (body) {
        var since = Date.now() - 3600 * 1000;
        var n = ((body && (body.tasks || body.items)) || []).filter(function (t) {
          var ts = Date.parse(t.updated_at || t.created_at || '');
          return !isNaN(ts) && ts >= since;
        }).length;
        if (v) { v.textContent = String(n); }
      })
      .catch(function () { if (v) { v.textContent = '—'; } });
  }

  function load() {
    banner('', '');
    api.json('GET', '/api/stats')
      .then(function (s) { renderStats(s); loadTrend(); })
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
    window.addEventListener('hashchange', function () {
      var h = parseConsoleHash();
      if (!h) { return; }
      if (h.section !== 'overview') { setPane(h.section, h.params, true); }
      else { setPane('overview', null, true); }
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-cs]'), function (b) {
      b.addEventListener('click', function () {
        if (b.disabled) { return; }
        setPane(b.getAttribute('data-cs'));
      });
    });
    var tr = el('v2-cs-tasks-refresh'); if (tr) { tr.addEventListener('click', loadTasks); }
    var tq = el('v2-cs-tasks-q');
    if (tq) { tq.addEventListener('input', function () { tasks.q = tq.value.trim(); tasks.page = 1; renderTaskRows(); syncTaskHash(); }); }
    var ts = el('v2-cs-tasks-status');
    if (ts) { ts.addEventListener('change', function () { tasks.status = ts.value; tasks.page = 1; renderTaskRows(); syncTaskHash(); }); }
    var szSel = el('v2-cs-tasks-size');
    if (szSel) { szSel.addEventListener('change', function () { tasks.size = szSel.value; tasks.page = 1; renderTaskRows(); syncTaskHash(); }); }
    var so = el('v2-cs-tasks-sort');
    if (so) { so.addEventListener('change', function () { tasks.sort = so.value; renderTaskRows(); syncTaskHash(); }); }
    var od = el('v2-cs-tasks-order');
    if (od) { od.addEventListener('change', function () { tasks.order = od.value; renderTaskRows(); syncTaskHash(); }); }
    var prev = el('v2-cs-tasks-prev');
    if (prev) { prev.addEventListener('click', function () { tasks.page = Math.max(1, tasks.page - 1); renderTaskRows(); syncTaskHash(); }); }
    var next = el('v2-cs-tasks-next');
    if (next) { next.addEventListener('click', function () { tasks.page += 1; renderTaskRows(); syncTaskHash(); }); }
    var bRetry = el('v2-cs-tasks-retry'); if (bRetry) { bRetry.addEventListener('click', function () { batchTasks('retry'); }); }
    var bCancel = el('v2-cs-tasks-cancel'); if (bCancel) { bCancel.addEventListener('click', function () { batchTasks('cancel'); }); }
    var bExport = el('v2-cs-tasks-export'); if (bExport) { bExport.addEventListener('click', exportTasks); }
    var imp = el('v2-cs-tasks-import');
    if (imp) { imp.addEventListener('change', function () { importTasks(imp.files && imp.files[0]); imp.value = ''; }); }
    renderRecentOps();
    var ir = el('v2-cs-issues-refresh'); if (ir) { ir.addEventListener('click', loadIssues); }
    var iq = el('v2-cs-issues-q');
    if (iq) { iq.addEventListener('input', function () { issues.q = iq.value.trim(); renderIssues(visibleIssues()); }); }
    var ist = el('v2-cs-issues-status');
    if (ist) { ist.addEventListener('change', function () { issues.status = ist.value; renderIssues(visibleIssues()); }); }
    var iex = el('v2-cs-issues-export'); if (iex) { iex.addEventListener('click', exportIssues); }
    var iscan = el('v2-cs-issues-scan');
    if (iscan) {
      iscan.addEventListener('click', function () {
        iscan.disabled = true;
        api.json('POST', '/api/issues/scan', {})
          .then(function (res) {
            if (window.v2toast) {
              window.v2toast('ok', '已触发存储审查' + (res && res.reported != null ? '（登记 ' + res.reported + ' 项）' : ''));
            }
            loadIssues();
          })
          .catch(function (e) { banner('warn', '扫描失败：' + (e.message || e)); })
          .then(function () { iscan.disabled = false; });
      });
    }
    var h = parseConsoleHash();
    if (h) { setPane(h.section, h.params, true); }
    else if (isActive()) { load(); }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
