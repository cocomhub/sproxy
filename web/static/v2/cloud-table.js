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
      nameTd.textContent = t.filename || t.url || t.id;
      if (t.filename && t.url) { nameTd.title = t.url; }
      tr.appendChild(nameTd);

      var stTd = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + statusClass(t.status);
      pill.textContent = ctLabel(t.status);
      stTd.appendChild(pill);
      tr.appendChild(stTd);

      var pgTd = document.createElement('td');
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
      szTd.textContent = fmt ? fmt.formatSize(t.total_size) : '—';
      tr.appendChild(szTd);

      var upTd = document.createElement('td');
      upTd.className = 'num muted';
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
      if (t.status === 'pending' || t.status === 'downloading') {
        actTd.appendChild(actionBtn('取消', function () { fire('cancel'); }));
      }
      if (t.status === 'failed') {
        actTd.appendChild(actionBtn('重试', function () { fire('retry'); }));
      }
      if (t.status === 'completed' || t.status === 'cancelled' || t.status === 'failed') {
        actTd.appendChild(actionBtn('删除', function () { fire('delete'); }));
      }
      tr.appendChild(actTd);
      tbody.appendChild(tr);
    });
  }

  /** 统一执行任务动作（取消/重试/删除），供传输页与控制台复用。 */
  function perform(kind, task, api, done) {
    var path, opts;
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

  window.v2cloudTable = { render: render, statusClass: statusClass, perform: perform };
})();
