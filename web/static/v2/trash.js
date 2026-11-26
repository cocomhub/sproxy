// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 回收站：GET /api/trash（多卷汇总）、POST /api/trash/restore、POST /api/trash/empty。
// 条目 trash_rel 是恢复所需路径；volume 用于跨卷定位。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var VOL_KEY = 'sproxy_v2_trash_volume';
  var allEntries = [];

  /** 当前卷筛选（空 = 全部）；跨重渲染保持（localStorage）。 */
  function currentVolume() {
    try { return localStorage.getItem(VOL_KEY) || ''; } catch (e) { return ''; }
  }

  function setVolume(v) {
    try {
      if (v) { localStorage.setItem(VOL_KEY, v); } else { localStorage.removeItem(VOL_KEY); }
    } catch (e) { /* 忽略 */ }
  }

  /** 渲染卷下拉（保持用户选择；卷消失时回落「全部」）。 */
  function renderVolumeOptions(entries) {
    var sel = el('v2-trash-volume');
    if (!sel) { return; }
    var vols = volumeOptions(entries);
    var keep = currentVolume();
    if (keep && vols.indexOf(keep) < 0) { keep = ''; setVolume(''); }
    sel.textContent = '';
    var all = document.createElement('option');
    all.value = ''; all.textContent = '全部卷';
    sel.appendChild(all);
    vols.forEach(function (v) {
      var o = document.createElement('option');
      o.value = v; o.textContent = v;
      sel.appendChild(o);
    });
    sel.value = keep;
    sel.hidden = vols.length <= 1;  // 只有一个卷时下拉无意义
  }

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-trash-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function normalizeEntries(body) {
    var list = body && Array.isArray(body.entries) ? body.entries : [];
    return list.filter(Boolean).map(function (e) {
      return { rel: e.trash_rel || '', name: e.name || e.trash_rel || '', volume: e.volume || '' };
    });
  }

  /**
   * 汇总视图的卷下拉选项（保持首次出现序，去重）。
   * @param {Array<{volume:string}>} entries
   * @returns {string[]}
   */
  function volumeOptions(entries) {
    var seen = [];
    (entries || []).forEach(function (e) {
      var v = (e && e.volume) || '';
      if (v && seen.indexOf(v) < 0) { seen.push(v); }
    });
    return seen;
  }

  /** 按卷筛选（vol 为空 ⇒ 全部）。 */
  function filterByVolume(entries, vol) {
    var list = entries || [];
    if (!vol) { return list; }
    return list.filter(function (e) { return ((e && e.volume) || '') === vol; });
  }

  /**
   * Go duration 字符串 → 人类可读保留期（'168h0m0s' → '7 天'）。
   * 解析失败返回 ''（调用方据此隐藏提示，**不编造**天数——页面曾写死「保留 30 天」）。
   * @param {string} raw
   * @returns {string}
   */
  function formatRetention(raw) {
    var s = String(raw || '').trim();
    if (!s) { return ''; }
    var m = /^(?:(-?\d+)h)?(?:(-?\d+)m)?(?:(-?\d+)s)?$/.exec(s);
    if (!m || (m[1] === undefined && m[2] === undefined && m[3] === undefined)) { return ''; }
    var sec = (Number(m[1] || 0) * 3600) + (Number(m[2] || 0) * 60) + Number(m[3] || 0);
    if (!(sec > 0)) { return ''; }
    if (sec % 86400 === 0) { return (sec / 86400) + ' 天'; }
    if (sec % 3600 === 0) { return (sec / 3600) + ' 小时'; }
    if (sec % 60 === 0) { return (sec / 60) + ' 分钟'; }
    return sec + ' 秒';
  }

  function render(entries) {
    var tbody = el('v2-trash-body');
    if (!tbody) { return; }
    allEntries = entries || [];
    var vol = currentVolume();
    renderVolumeOptions(allEntries);
    var rows = filterByVolume(allEntries, vol);
    tbody.textContent = '';
    rows.forEach(function (e) {
      var tr = document.createElement('tr');
      var name = document.createElement('td');
      name.className = 'mono'; name.textContent = e.name; name.title = e.rel;
      name.setAttribute('data-label', '名称');
      tr.appendChild(name);
      var vol = document.createElement('td');
      vol.setAttribute('data-label', '卷');
      if (e.volume) {
        var pill = document.createElement('span');
        pill.className = 'pill pill-info'; pill.textContent = e.volume;
        vol.appendChild(pill);
      } else { vol.textContent = '—'; vol.className = 'muted'; }
      tr.appendChild(vol);
      var act = document.createElement('td');
      act.className = 'r actions';
      var btn = document.createElement('button');
      btn.type = 'button'; btn.className = 'btn btn-ghost btn-sm'; btn.textContent = '恢复';
      btn.setAttribute('data-trash-restore', e.rel);
      btn.addEventListener('click', function () { restore(e); });
      act.appendChild(btn);
      // 单条彻底删除：回收站内的删除**恒为硬删**（不受「默认删除方式」影响）。
      var del = document.createElement('button');
      del.type = 'button'; del.className = 'btn btn-ghost btn-sm'; del.textContent = '彻底删除';
      del.setAttribute('data-trash-purge', e.rel);
      del.addEventListener('click', function () { purge(e); });
      act.appendChild(del);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    var hint = el('v2-trash-empty-hint');
    if (hint) { hint.hidden = rows.length > 0; }
    var count = el('v2-trash-count');
    if (count) {
      count.textContent = vol
        ? (rows.length + ' / ' + allEntries.length + ' 项（卷 ' + vol + '）')
        : (allEntries.length + ' 项');
    }
    try {
      document.dispatchEvent(new CustomEvent('v2:badge', { detail: { route: 'trash', count: entries.length } }));
      document.dispatchEvent(new CustomEvent('v2:subtitle', {
        detail: { route: 'trash', text: '/ ' + entries.length + ' 个可恢复文件' }
      }));
    } catch (e) { /* 忽略 */ }
  }

  function load() {
    if (!api) { return Promise.resolve(); }
    banner('');
    return api.json('GET', '/api/trash')
      .then(function (body) { render(normalizeEntries(body)); })
      .catch(function (e) { render([]); banner('回收站读取失败：' + (e.message || e), 'warn'); });
  }

  function restore(entry) {
    var q = '?file=' + encodeURIComponent(entry.rel);
    if (entry.volume) { q += '&volume=' + encodeURIComponent(entry.volume); }
    api.json('POST', '/api/trash/restore' + q, {})
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已恢复 ' + entry.name); }
        load();
        if (window.v2filesView) { window.v2filesView.load(); }
      })
      .catch(function (e) { banner('恢复失败：' + (e.message || e), 'warn'); });
  }

  /** 单条彻底删除（回收站内删除恒为硬删，不受「默认删除方式」影响）。 */
  function purge(entry) {
    // 与文件页删除共用「跳过二次确认」开关（它就是删除确认的一部分）。
    var skip = window.v2deleteMode && window.v2deleteMode.skipConfirm && window.v2deleteMode.skipConfirm();
    if (!skip && !window.confirm('彻底删除 ' + entry.name + '？此操作不可恢复。')) { return; }
    var q = '?file=' + encodeURIComponent(entry.rel);
    if (entry.volume) { q += '&volume=' + encodeURIComponent(entry.volume); }
    api.json('DELETE', '/api/trash' + q)
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已彻底删除 ' + entry.name); }
        load();
      })
      .catch(function (e) { banner('彻底删除失败：' + (e.message || e), 'warn'); });
  }

  /**
   * 保留期提示取自 GET /api/config 的 `trash_retention`（实际生效值）。
   * 此前页面写死「保留 30 天」，而服务端默认是 7 天——文案与行为矛盾，属误导。
   * 读不到（老服务端/无权限）⇒ 保留**不含天数**的通用文案，绝不编造数字。
   */
  function loadRetention() {
    if (!api) { return; }
    api.json('GET', '/api/config')
      .then(function (cfg) {
        var human = formatRetention(cfg && cfg.trash_retention);
        var box = el('v2-trash-retention');
        if (box && human) {
          box.textContent = '删除的文件可在此恢复；超过 ' + human + ' 后由服务端自动清理。';
        }
      })
      .catch(function () { /* 保留通用文案 */ });
  }

  function emptyAll() {
    if (!window.confirm('清空全部卷的回收站？此操作不可撤销。')) { return; }
    api.json('POST', '/api/trash/empty?all=1', {})
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '回收站已清空'); }
        load();
      })
      .catch(function (e) { banner('清空失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var vol = el('v2-trash-volume');
    if (vol) {
      vol.addEventListener('change', function () {
        setVolume(vol.value);
        render(allEntries);
      });
    }
    var r = el('v2-trash-refresh'); if (r) { r.addEventListener('click', load); }
    loadRetention();
    var e = el('v2-trash-empty'); if (e) { e.addEventListener('click', emptyAll); }
    if (el('v2-trash-body')) { load(); }
  }

  function maybeInit() {
    if ((window.location.hash || '').indexOf('#/trash') === 0) { init(); }
  }

  if (typeof window !== 'undefined') {
    window.v2trash = { init: init, load: load };
    window.addEventListener('hashchange', maybeInit);
    maybeInit();
  }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      normalizeEntries: normalizeEntries,
      volumeOptions: volumeOptions,
      filterByVolume: filterByVolume,
      formatRetention: formatRetention
    };
  }
})();
