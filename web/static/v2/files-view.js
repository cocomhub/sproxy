// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件页（P1a/P1b）：列表渲染 + 选择模型；写操作委托 files-actions.js。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }

  var auth = window.v2auth;
  var f = window.v2files;
  if (!auth || !f) { return; }

  var VOL_KEY = 'sproxy_v2_volume';
  var SUBDIR_KEY = 'sproxy_v2_subdir_by_vol';
  var PAGE = 200;
  var state = {
    subdir: '', volume: '', query: '', files: [], selected: Object.create(null),
    sort: 'name', order: 'asc', limit: PAGE, offset: 0, total: -1, hasMore: false, loading: false
  };

  function el(id) { return document.getElementById(id); }

  /** 广播侧栏徽标（nav-badges.js 监听；未加载时静默）。 */
  function badge(route, count) {
    try {
      document.dispatchEvent(new CustomEvent('v2:badge', { detail: { route: route, count: count } }));
    } catch (e) { /* 忽略 */ }
  }

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
      body.appendChild(buildRow(subdir, file));
    });
  }

  /** 追加一页（「加载更多」），不重建已有行（保留滚动位置与勾选）。 */
  function appendRows(subdir, files) {
    var body = el('v2-file-body');
    if (!body) { return; }
    files.forEach(function (file) {
      body.appendChild(buildRow(subdir, file));
    });
  }

  /** 构建单行。 */
  function buildRow(subdir, file) {
    var tr = document.createElement('tr');
    (function () {
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
      link.href = file.isDir ? hashForSubdir(state.query ? file.name : f.joinSubdir(subdir, file.name)) : '#';
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
      } else {
        actTd.appendChild(actionBtn('打包下载', file.name, 'archive'));
      }
      // 目录也支持重命名/移动（服务端已支持整棵子树搬迁，无需 checksum）。
      actTd.appendChild(actionBtn('重命名', file.name, 'rename'));
      actTd.appendChild(actionBtn('删除', file.name, 'delete'));
      tr.appendChild(actTd);

      tr.setAttribute('data-name', file.name);
      tr.setAttribute('data-dir', file.isDir ? '1' : '');
      tr.setAttribute('data-checksum', file.checksum || '');
    })();
    return tr;
  }

  function syncSelection() {
    var n = Object.keys(state.selected).length;
    ['v2-batch-delete', 'v2-batch-rename', 'v2-batch-archive'].forEach(function (id) {
      var b = el(id);
      if (b) { b.disabled = n === 0; }
    });
    var all = el('v2-select-all');
    if (all) { all.checked = n > 0 && n === state.files.length; }
    var count = el('v2-count');
    if (count) { count.textContent = countText(n); }
    badge('files', state.files.length);
    renderSortMarks();
  }

  /** 计数文案：有 total 时「已加载 X / 共 Y」，否则只有已加载数（不编造总数）。 */
  function countText(selected) {
    var loaded = state.files.length;
    var base = state.total >= 0 ? (loaded + ' / ' + state.total + ' 项') : (loaded + ' 项');
    return base + (selected ? ' · 已选 ' + selected : '');
  }

  function renderSortMarks() {
    ['name', 'size', 'time'].forEach(function (fld) {
      var n = el('v2-sort-' + fld);
      if (n) { n.textContent = f.sortMark({ sort: state.sort, order: state.order }, fld); }
    });
    Array.prototype.forEach.call(document.querySelectorAll('th.sortable'), function (th) {
      var active = th.getAttribute('data-sort') === state.sort;
      th.setAttribute('aria-sort', active ? (state.order === 'desc' ? 'descending' : 'ascending') : 'none');
    });
  }

  /** 「加载更多」区：仅当服务端还有更多时显示，并说明已加载/总数。 */
  function renderLoadMore() {
    var wrap = el('v2-load-more-wrap');
    var hint = el('v2-load-more-hint');
    if (!wrap) { return; }
    wrap.hidden = !state.hasMore;
    if (hint) {
      hint.textContent = state.hasMore
        ? ('已加载 ' + state.files.length + (state.total >= 0 ? ' / ' + state.total : '') + ' 项')
        : '';
    }
  }

  function listPath() {
    return f.listURL({
      subdir: state.subdir, volume: state.volume, query: state.query,
      sort: state.sort, order: state.order, limit: state.limit, offset: state.offset
    });
  }

  /** 按卷记忆子目录（切换卷时回到该卷上次目录；无记录则根）。 */
  function subdirMap() {
    try { return JSON.parse(localStorage.getItem(SUBDIR_KEY) || '{}') || {}; } catch (e) { return {}; }
  }

  function rememberSubdir(vol, subdir) {
    var m = subdirMap();
    if (subdir) { m[vol || ''] = subdir; } else { delete m[vol || '']; }
    try { localStorage.setItem(SUBDIR_KEY, JSON.stringify(m)); } catch (e) { /* 忽略 */ }
  }

  function subdirForVolume(vol) {
    return subdirMap()[vol || ''] || '';
  }

  /**
   * 加载列表。
   * @param {boolean} [reset] 默认 true：重置分页到第一页（切换目录/排序/卷/搜索时）；
   *   「加载更多」传 false（沿用已递增的 offset 追加）。
   */
  function load(reset) {
    if (reset !== false) { state.offset = 0; }
    var subdir = subdirFromHash();
    if (state.volume && subdir === '' && !window.__v2volSwitch) {
      subdir = subdirForVolume(state.volume);
    }
    window.__v2volSwitch = false;
    if (state.volume && subdir) { rememberSubdir(state.volume, subdir); }
    state.subdir = subdir;
    renderCrumbs(subdir);
    setState('', '');
    var append = state.offset > 0;
    var body = el('v2-file-body');
    if (body && !append) { body.textContent = ''; }
    var count = el('v2-count');
    if (count && !append) { count.textContent = '加载中…'; }

    var path = listPath();
    var api = window.v2api;
    state.loading = true;
    (api ? api.json('GET', path) : Promise.reject(new Error('api 未装配')))
      .then(function (payload) {
        var meta = f.parseListMeta(payload);
        var page = f.normalizeFiles(payload);
        state.total = meta.total;
        state.hasMore = meta.hasMore;
        state.files = append ? state.files.concat(page) : page;
        state.loading = false;
        if (append) { appendRows(subdir, page); } else { renderRows(subdir, state.files); }
        syncSelection();
        renderLoadMore();
        if (!state.files.length) {
          setState('empty', state.query ? ('没有匹配「' + state.query + '」的文件。') : '此目录为空，可上传文件或新建目录。');
        }
      })
      .catch(function (err) {
        state.loading = false;
        if (count) { count.textContent = '—'; }
        badge('files', null);
        if (!append) { state.files = []; }
        renderLoadMore();
        setState('error', '加载失败：' + (err && err.message ? err.message : '网络错误') + '（可在旧版确认登录状态）');
      });
  }

  /** 强制刷新（清卷状态缓存）后重载列表；可针对当前卷。 */
  function forceRefresh() {
    var api = window.v2api;
    if (!api) { return; }
    var path = '/api/volumes/refresh' + (state.volume ? '?volume=' + encodeURIComponent(state.volume) : '');
    api.json('POST', path).then(function () {
      if (window.v2toast) { window.v2toast('ok', '已刷新卷状态'); }
      load();
    }).catch(function (e) {
      if (window.v2toast) { window.v2toast('bad', '刷新失败：' + (e.message || e)); }
    });
  }

  function loadVolumes() {
    var sel = el('v2-volume');
    var api = window.v2api;
    if (!sel || !api) { return; }
    api.json('GET', '/api/volumes').then(function (body) {
      var vols = (body && body.volumes) || [];
      sel.textContent = '';
      var all = document.createElement('option');
      all.value = ''; all.textContent = '全部卷';
      sel.appendChild(all);
      vols.forEach(function (v) {
        if (!v || !v.name) { return; }
        var o = document.createElement('option');
        o.value = v.name; o.textContent = v.name;
        sel.appendChild(o);
      });
      try { state.volume = localStorage.getItem(VOL_KEY) || ''; } catch (e) { state.volume = ''; }
      sel.value = state.volume;
    }).catch(function () { /* 列表仍可用，仅无卷过滤 */ });
  }

  function applySort(field) {
    var next = f.nextSort({ sort: state.sort, order: state.order }, field);
    state.sort = next.sort;
    state.order = next.order;
    load();
  }

  function init() {
    Array.prototype.forEach.call(document.querySelectorAll('th.sortable'), function (th) {
      var field = th.getAttribute('data-sort');
      th.addEventListener('click', function () { applySort(field); });
      th.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); applySort(field); }
      });
    });
    var more = el('v2-load-more');
    if (more) {
      more.addEventListener('click', function () {
        if (state.loading) { return; }
        state.offset += state.limit;
        load(false);
      });
    }

    var refresh = el('v2-refresh');
    if (refresh) { refresh.addEventListener('click', function () { load(); }); }
    var volRefresh = el('v2-vol-refresh');
    if (volRefresh) { volRefresh.addEventListener('click', forceRefresh); }

    var vol = el('v2-volume');
    if (vol) {
      vol.addEventListener('change', function () {
        state.volume = vol.value;
        try { localStorage.setItem(VOL_KEY, state.volume); } catch (e) { /* 忽略 */ }
        // 切卷：跳回该卷上次目录（root 时清 hash 子路径）。
        window.__v2volSwitch = true;
        var target = subdirForVolume(state.volume);
        try { history.replaceState(null, '', target ? '#/files/' + encodeURIComponent(target) : '#/files'); } catch (e) { /* 忽略 */ }
        load();
      });
    }
    var search = el('v2-search');
    var searchBtn = el('v2-search-btn');
    var searchClear = el('v2-search-clear');
    function runSearch() {
      state.query = ((search && search.value) || '').trim();
      if (searchClear) { searchClear.hidden = !state.query; }
      load();
    }
    if (search) {
      search.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); runSearch(); } });
    }
    if (searchBtn) { searchBtn.addEventListener('click', runSearch); }
    if (searchClear) {
      searchClear.addEventListener('click', function () {
        if (search) { search.value = ''; }
        runSearch();
      });
    }
    loadVolumes();

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
    volume: function () { return state.volume; },
    search: function () { return state.query; },
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
