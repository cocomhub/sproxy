// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件页（P1a/P1b）：列表渲染 + 选择模型；写操作委托 files-actions.js。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }

  var auth = window.v2auth;
  var f = window.v2files;
  if (!auth || !f) { return; }

  var state = { subdir: '', files: [], selected: Object.create(null) };

  function el(id) { return document.getElementById(id); }

  function subdirFromHash() {
    var raw = String(window.location.hash || '').replace(/^#\/?/, '');
    var parts = raw.split('/');
    if (parts[0] !== 'files') { return ''; }
    try { return decodeURIComponent(parts.slice(1).join('/')); } catch (e) { return ''; }
  }

  function hashForSubdir(subdir) {
    return subdir ? '#/files/' + encodeURIComponent(subdir) : '#/files';
  }

  function setState(kind, message) {
    var box = el('v2-files-state');
    if (!box) { return; }
    if (!kind) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = message;
  }

  function renderCrumbs(subdir) {
    var box = el('v2-crumbs');
    if (!box) { return; }
    box.textContent = '';
    f.crumbsFor(subdir).forEach(function (c, i, all) {
      if (i > 0) {
        var sep = document.createElement('span');
        sep.className = 'muted';
        sep.textContent = ' / ';
        box.appendChild(sep);
      }
      var a = document.createElement('a');
      a.href = hashForSubdir(c.path);
      a.textContent = c.label;
      if (i === all.length - 1) { a.setAttribute('aria-current', 'page'); }
      box.appendChild(a);
    });
  }

  function cell(text, cls) {
    var td = document.createElement('td');
    td.textContent = text;
    if (cls) { td.className = cls; }
    return td;
  }

  function actionBtn(label, name, action) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn-ghost btn-sm';
    b.textContent = label;
    b.setAttribute('data-file', name);
    b.setAttribute('data-action', action);
    return b;
  }

  function renderRows(subdir, files) {
    var body = el('v2-file-body');
    if (!body) { return; }
    body.textContent = '';
    state.selected = Object.create(null);
    files.forEach(function (file) {
      var tr = document.createElement('tr');
      if (file.isDir) { tr.className = 'is-dir'; }

      var pickTd = document.createElement('td');
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.className = 'cb';
      cb.setAttribute('aria-label', '选择 ' + file.name);
      cb.setAttribute('data-name', file.name);
      pickTd.appendChild(cb);
      tr.appendChild(pickTd);

      var nameTd = document.createElement('td');
      var link = document.createElement('a');
      link.href = file.isDir ? hashForSubdir(f.joinSubdir(subdir, file.name)) : '#';
      if (!file.isDir) { link.setAttribute('data-preview', file.name); }
      link.textContent = file.name;
      link.className = 'mono';
      nameTd.appendChild(link);
      if (file.isDir) {
        var tag = document.createElement('span');
        tag.className = 'pill pill-mute';
        tag.textContent = '目录';
        nameTd.appendChild(tag);
      }
      tr.appendChild(nameTd);

      var volTd = document.createElement('td');
      if (file.volume) {
        var pill = document.createElement('span');
        pill.className = 'pill ' + (file.volumeCategory === 'wrapper' ? 'pill-warn' : 'pill-info');
        pill.textContent = file.volume;
        volTd.appendChild(pill);
      } else {
        volTd.textContent = '—';
        volTd.className = 'muted';
      }
      tr.appendChild(volTd);

      tr.appendChild(cell(file.isDir ? '—' : f.formatSize(file.size), 'r num'));
      tr.appendChild(cell(f.formatTime(file.modTime), 'num muted'));
      tr.appendChild(cell(file.checksum ? '有' : '—', file.checksum ? '' : 'muted'));

      var actTd = document.createElement('td');
      actTd.className = 'r actions';
      actTd.appendChild(actionBtn('详情', file.name, 'detail'));
      if (!file.isDir) {
        actTd.appendChild(actionBtn('下载', file.name, 'download'));
        actTd.appendChild(actionBtn('重命名', file.name, 'rename'));
      }
      actTd.appendChild(actionBtn('删除', file.name, 'delete'));
      tr.appendChild(actTd);

      tr.setAttribute('data-name', file.name);
      tr.setAttribute('data-dir', file.isDir ? '1' : '');
      tr.setAttribute('data-checksum', file.checksum || '');
      body.appendChild(tr);
    });
  }

  function syncSelection() {
    var n = Object.keys(state.selected).length;
    var btn = el('v2-batch-delete');
    if (btn) { btn.disabled = n === 0; }
    var all = el('v2-select-all');
    if (all) { all.checked = n > 0 && n === state.files.length; }
    var count = el('v2-count');
    if (count) { count.textContent = state.files.length + ' 项' + (n ? ' · 已选 ' + n : ''); }
  }

  function load() {
    var subdir = subdirFromHash();
    state.subdir = subdir;
    renderCrumbs(subdir);
    setState('', '');
    var body = el('v2-file-body');
    if (body) { body.textContent = ''; }
    var count = el('v2-count');
    if (count) { count.textContent = '加载中…'; }

    var path = '/api/files?subdir=' + encodeURIComponent(subdir) + '&limit=200';
    var creds = auth.loadCreds(window.sessionStorage);
    auth.authHeaders('GET', path, null, creds, window.sclientSig && window.sclientSig.signHeader)
      .then(function (headers) { return fetch(path, { headers: headers, credentials: 'same-origin' }); })
      .then(function (resp) {
        if (resp.status === 401) { throw new Error('未登录：请先在旧版填写 AccessKey/Secret'); }
        if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
        return resp.json();
      })
      .then(function (payload) {
        state.files = f.normalizeFiles(payload);
        renderRows(subdir, state.files);
        syncSelection();
        if (!state.files.length) { setState('empty', '此目录为空，可上传文件或新建目录。'); }
      })
      .catch(function (err) {
        if (count) { count.textContent = '—'; }
        state.files = [];
        setState('error', '加载失败：' + (err && err.message ? err.message : '网络错误') + '（可在旧版确认登录状态）');
      });
  }

  function init() {
    var refresh = el('v2-refresh');
    if (refresh) { refresh.addEventListener('click', load); }

    var body = el('v2-file-body');
    if (body) {
      body.addEventListener('click', function (e) {
        var pv = e.target.closest('[data-preview]');
        if (pv && window.v2preview) { e.preventDefault(); window.v2preview.open(pv.getAttribute('data-preview')); }
      });
      body.addEventListener('change', function (e) {
        var cb = e.target.closest('.cb');
        if (!cb) { return; }
        var name = cb.getAttribute('data-name');
        if (cb.checked) { state.selected[name] = true; } else { delete state.selected[name]; }
        syncSelection();
      });
    }
    var all = el('v2-select-all');
    if (all) {
      all.addEventListener('change', function () {
        state.selected = Object.create(null);
        Array.prototype.forEach.call(document.querySelectorAll('#v2-file-body .cb'), function (cb) {
          cb.checked = all.checked;
          if (all.checked) { state.selected[cb.getAttribute('data-name')] = true; }
        });
        syncSelection();
      });
    }

    window.addEventListener('hashchange', function () {
      if (window.location.hash.indexOf('#/files') === 0) { load(); }
    });
    load();
  }

  window.v2filesView = {
    load: load,
    state: state,
    subdir: function () { return state.subdir; },
    selected: function () { return Object.keys(state.selected); },
    file: function (name) {
      for (var i = 0; i < state.files.length; i += 1) {
        if (state.files[i].name === name) { return state.files[i]; }
      }
      return null;
    },
    rowInfo: function (name) {
      var tr = document.querySelector('#v2-file-body tr[data-name="' + String(name).replace(/"/g, '\\"') + '"]');
      if (!tr) { return null; }
      return { name: name, isDir: tr.getAttribute('data-dir') === '1', checksum: tr.getAttribute('data-checksum') || '' };
    }
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
