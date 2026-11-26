// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 云端任务表格渲染（单一实现）：传输页与控制台「云端任务」分区共用。
// 只做 DOM 行渲染；操作以回调交给调用方（取消/重试/删除）。
'use strict';

(function () {
  if (typeof window === 'undefined') { return; }
  var fmt = window.sharedFormat;
  var ctLabel = function (s) { return (window.ctStatusLabel ? window.ctStatusLabel(s) : (s || '—')); };
  var ctPct = function (t) { return (window.ctProgressPercent ? window.ctProgressPercent(t) : null); };

  function statusClass(s) {
    if (s === 'completed') { return 'pill-ok'; }
    if (s === 'failed') { return 'pill-bad'; }
    if (s === 'cancelled') { return 'pill-mute'; }
    return 'pill-info';
  }

  /** 完整性徽标：verified（通过）/ damaged（损坏）/ 空（未校验）。 */
  function integrityBadge(t) {
    var st = t && t.integrity_status;
    if (!st) { return null; }
    var span = document.createElement('span');
    span.className = 'pill ' + (st === 'verified' ? 'pill-ok' : (st === 'damaged' ? 'pill-bad' : 'pill-mute'));
    span.style.marginLeft = '6px';
    span.textContent = st === 'verified' ? '完整' : (st === 'damaged' ? '损坏' : st);
    span.setAttribute('data-integrity', st);
    return span;
  }

  /**
   * 断点徽标：cancelled/failed 且服务端记录了 PartialSize ⇒ 显示「断点 X（可续传）」。
   *
   * 为什么重要：取消/失败后的任务**保留 .partial 断点**（2026-11-26 语义：取消=中断），
   * 续传会从断点继续；此前 UI 只显示「失败」+ 一个笼统「重试」，用户无法判断重试是**续传**
   * 还是**从零重下**（后者在慢源上代价是几小时）。
   */
  function resumeBadge(t) {
    var d = t && t.diagnostics;
    if (!d || !d.partial_size || d.partial_size <= 0) { return null; }
    if (t.status !== 'cancelled' && t.status !== 'failed') { return null; }
    var span = document.createElement('span');
    span.className = 'pill pill-info';
    span.style.marginLeft = '6px';
    span.setAttribute('data-resume', '1');
    span.title = '已保留断点，重试将从断点继续（不重下已完成块）';
    span.textContent = '断点 ' + (fmt ? fmt.formatSize(d.partial_size) : d.partial_size);
    return span;
  }

  function actionBtn(text, handler) {
    var b = document.createElement('button');
    b.type = 'button'; b.className = 'btn btn-sm btn-ghost'; b.textContent = text;
    b.addEventListener('click', handler);
    return b;
  }

  /**
   * @param {HTMLElement} tbody
   * @param {Array} tasks
   * @param {{onAction?:function(string, object)}} [opts]
   */
  function render(tbody, tasks, opts) {
    if (!tbody) { return; }
    var o = opts || {};
    tbody.textContent = '';
    (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
      var tr = document.createElement('tr');
      if (o.selectable) {
        var pickTd = document.createElement('td');
        var cb = document.createElement('input');
        cb.type = 'checkbox'; cb.className = 'cb';
        cb.setAttribute('aria-label', '选择任务 ' + (t.filename || t.id));
        cb.checked = !!(o.isSelected && o.isSelected(t));
        cb.addEventListener('change', function () {
          if (o.onToggle) { o.onToggle(t, cb.checked); }
        });
        pickTd.appendChild(cb);
        tr.appendChild(pickTd);
      }
      var nameTd = document.createElement('td');
      nameTd.className = 'mono';
      nameTd.setAttribute('data-label', '任务');
      nameTd.textContent = t.filename || t.url || t.id;
      if (t.filename && t.url) { nameTd.title = t.url; }
      tr.appendChild(nameTd);

      var stTd = document.createElement('td');
      stTd.setAttribute('data-label', '状态');
      var pill = document.createElement('span');
      pill.className = 'pill ' + statusClass(t.status);
      pill.textContent = ctLabel(t.status);
      stTd.appendChild(pill);
      var badge = integrityBadge(t);
      if (badge) { stTd.appendChild(badge); }
      var rb = resumeBadge(t);
      if (rb) { stTd.appendChild(rb); }
      tr.appendChild(stTd);

      var pgTd = document.createElement('td');
      pgTd.setAttribute('data-label', '进度');
      var p = ctPct(t);
      var bar = document.createElement('div');
      bar.className = 'bar' + (t.status === 'failed' ? ' bad' : '');
      var fill = document.createElement('i');
      fill.style.width = (p == null ? 0 : p) + '%';
      bar.appendChild(fill);
      var txt = document.createElement('div');
      txt.className = 'muted'; txt.style.fontSize = '11px';
      txt.textContent = p == null ? '—' : (p + '%');
      pgTd.appendChild(bar); pgTd.appendChild(txt);
      tr.appendChild(pgTd);

      var szTd = document.createElement('td');
      szTd.className = 'r num';
      szTd.setAttribute('data-label', '大小');
      szTd.textContent = fmt ? fmt.formatSize(t.total_size) : '—';
      tr.appendChild(szTd);

      var upTd = document.createElement('td');
      upTd.className = 'num muted';
      upTd.setAttribute('data-label', '更新');
      upTd.textContent = t.updated_at && fmt ? fmt.formatTime(Date.parse(t.updated_at)) : '—';
      tr.appendChild(upTd);

      var actTd = document.createElement('td');
      actTd.className = 'r actions';
      var fire = function (kind) {
        if (o.onAction) { o.onAction(kind, t); }
      };
      if (o.onDetail) {
        actTd.appendChild(actionBtn('详情', function () { o.onDetail(t); }));
      }
      if (o.onPreview && t.status === 'completed') {
        actTd.appendChild(actionBtn('预览', function () { o.onPreview(t); }));
      }
      // **服务端 action plan 优先**：可用性判据（有无可续断点 / 有无备选源 / 产物是否完整）
      // 只有服务端知道；此前前端硬编码「重试」会把必然失败或无意义的动作也渲染出来。
      //
      // 注意：plan 只替代「重试/取消」这类**状态相关**动作，**不能**吞掉通用动作
      // （详情/预览/删除）—— 控制台的「详情」就依赖 onDetail，早期版本在这里提前 return
      // 直接把详情按钮弄没了（e2e「任务详情未渲染」抓到的真实回归）。
      var hasPlan = Array.isArray(t.actions) && t.actions.length > 0;
      if (hasPlan) {
        t.actions.forEach(function (plan) {
          if (!plan || !plan.action) { return; }
          var b = document.createElement('button');
          b.type = 'button';
          b.className = 'btn btn-sm ' + (plan.disabled ? 'btn-ghost' : '');
          b.textContent = plan.label || plan.action;
          b.setAttribute('data-plan-action', plan.action);
          if (plan.disabled) {
            b.disabled = true;
            b.title = plan.reason || '当前不可用';
          } else if (plan.hint) {
            b.title = plan.hint;
          }
          b.addEventListener('click', function () { fire(plan.action); });
          actTd.appendChild(b);
        });
      }
      if (!hasPlan && (t.status === 'pending' || t.status === 'downloading')) {
        actTd.appendChild(actionBtn('取消', function () { fire('cancel'); }));
      }
      if (!hasPlan && t.status === 'failed') {
        actTd.appendChild(actionBtn('重试', function () { fire('retry'); }));
      }
      if (t.status === 'completed' || t.status === 'cancelled' || t.status === 'failed') {
        actTd.appendChild(actionBtn('删除', function () { fire('delete'); }));
      }
      tr.appendChild(actTd);
      tbody.appendChild(tr);
    });
  }

  /**
   * 服务端 action plan 里出现的动作集合（除 cancel/delete 外都走统一端点）。
   * `needs_url` 的动作需要用户补一个 URL（换链接重试）。
   */
  var PLAN_ACTIONS = ['retry_download', 'retry_transfer', 'retry_transfer_overwrite',
    'rotate_source', 'discard_partial', 'retry_from_url'];

  /** 统一执行任务动作（取消 / 服务端 plan 动作 / 重试 / 删除），供传输页与控制台复用。 */
  function perform(kind, task, api, done, extraURL) {
    var path, opts;
    if (PLAN_ACTIONS.indexOf(kind) >= 0) {
      var body = { action: kind };
      if (kind === 'retry_from_url') {
        if (!extraURL) {
          if (done) { done(false, new Error('换链接重试需要提供新的下载链接')); }
          return Promise.resolve();
        }
        body.url = extraURL;
      }
      return api.json('POST', '/api/cloud/tasks/' + encodeURIComponent(task.id) + '/actions', {
        body: new TextEncoder().encode(JSON.stringify(body)),
        contentType: 'application/json'
      }).then(function () { if (done) { done(true, kind); } })
        .catch(function (e) { if (done) { done(false, e); } });
    }
    if (kind === 'cancel') {
      path = '/api/cloud/tasks/' + encodeURIComponent(task.id) + '/cancel';
      opts = {};
    } else if (kind === 'retry') {
      path = '/api/cloud/tasks/retry';
      opts = {
        body: new TextEncoder().encode(JSON.stringify({ task_id: task.id, force: true })),
        contentType: 'application/json'
      };
    } else if (kind === 'delete') {
      path = '/api/cloud/tasks/' + encodeURIComponent(task.id);
      opts = undefined;
    } else {
      return Promise.resolve();
    }
    var method = kind === 'delete' ? 'DELETE' : 'POST';
    return api.json(method, path, opts).then(function () { if (done) { done(true, kind); } })
      .catch(function (e) { if (done) { done(false, e); } });
  }

  window.v2cloudTable = {
    render: render, statusClass: statusClass, perform: perform,
    integrityBadge: integrityBadge, resumeBadge: resumeBadge,
    PLAN_ACTIONS: PLAN_ACTIONS
  };
})();
