// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件页（P1a）：调用 GET /api/files（复用旧版 SproxySig 凭据），渲染列表与面包屑。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }

  var auth = window.v2auth;
  var f = window.v2files;
  if (!auth || !f) { return; }

  function el(id) { return document.getElementById(id); }

  function subdirFromHash() {
    var raw = String(window.location.hash || '').replace(/^#\/?/, '');
    var parts = raw.split('/');
    if (parts[0] !== 'files') { return ''; }
    try { return decodeURIComponent(parts.slice(1).join('/')); } catch (e) { return ''; }
  }

  function hashForSubdir(subdir) {
    var base = '#/files';
    return subdir ? base + '/' + encodeURIComponent(subdir) : base;
  }

  function setState(kind, message) {
    var box = el('v2-files-state');
    var body = el('v2-file-body');
    if (!box || !body) { return; }
    if (!kind) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'banner' + (kind === 'error' ? ' banner-warn' : '');
    box.textContent = message;
    if (kind === 'error') { body.textContent = ''; }
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

  function renderRows(subdir, files) {
    var body = el('v2-file-body');
    if (!body) { return; }
    body.textContent = '';
    files.forEach(function (file) {
      var tr = document.createElement('tr');
      if (file.isDir) { tr.className = 'is-dir'; }

      var nameTd = document.createElement('td');
      var link = document.createElement('a');
      link.href = file.isDir ? hashForSubdir(f.joinSubdir(subdir, file.name))
        : '/download?filename=' + encodeURIComponent(f.joinSubdir(subdir, file.name));
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
      var ck = cell(file.checksum ? '有' : '—', file.checksum ? '' : 'muted');
      tr.appendChild(ck);
      body.appendChild(tr);
    });
  }

  function load() {
    var subdir = subdirFromHash();
    renderCrumbs(subdir);
    setState('', '');
    var body = el('v2-file-body');
    var count = el('v2-count');
    if (body) { body.textContent = ''; }
    if (count) { count.textContent = '加载中…'; }

    var path = '/api/files?subdir=' + encodeURIComponent(subdir) + '&limit=200';
    var creds = auth.loadCreds(window.sessionStorage);
    auth.authHeaders('GET', path, null, creds, window.sclientSig && window.sclientSig.signHeader)
      .then(function (headers) {
        return fetch(path, { headers: headers, credentials: 'same-origin' });
      })
      .then(function (resp) {
        if (resp.status === 401) { throw new Error('未登录：请先在旧版填写 AccessKey/Secret'); }
        if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
        return resp.json();
      })
      .then(function (payload) {
        var files = f.normalizeFiles(payload);
        renderRows(subdir, files);
        if (count) { count.textContent = String(files.length) + ' 项'; }
        if (!files.length) { setState('empty', '此目录为空。上传与新建目录请使用旧版。'); }
      })
      .catch(function (err) {
        if (count) { count.textContent = '—'; }
        setState('error', '加载失败：' + (err && err.message ? err.message : '网络错误') + '（可在旧版确认登录状态）');
      });
  }

  function init() {
    var refresh = el('v2-refresh');
    if (refresh) { refresh.addEventListener('click', load); }
    window.addEventListener('hashchange', function () {
      if (subdirFromHash() !== null && window.location.hash.indexOf('#/files') === 0) { load(); }
    });
    load();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
