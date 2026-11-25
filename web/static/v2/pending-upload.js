// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 未完成上传续传：记录上次分块上传会话（localStorage），刷新后提示「重新选择同一文件继续」；
// Chromium 额外用 File System Access API 记住文件句柄（IndexedDB），可一键自动续传。
// 服务端以 filename|size|mtime|checksum 派生 upload_id，同一文件重新 init 会返回已收分块，
// 客户端据 /upload/status 的 missing_chunks 只补缺口。
'use strict';

(function () {
  var KEY = 'sproxy_v2_pending_upload';
  var DB = 'sproxy_v2_handles';
  var STORE = 'handles';

  function load() {
    try {
      var raw = localStorage.getItem(KEY);
      return raw ? JSON.parse(raw) : null;
    } catch (e) { return null; }
  }

  function save(rec) {
    try { localStorage.setItem(KEY, JSON.stringify(rec)); } catch (e) { /* 忽略 */ }
  }

  function clear() {
    try { localStorage.removeItem(KEY); } catch (e) { /* 忽略 */ }
    idb(function (db) { db.delete(STORE, 'pending'); }).catch(function () {});
    render();
  }

  /** 同文件判据：基名 + 大小 + mtime（毫秒）。 */
  function sameFile(rec, file) {
    if (!rec || !file) { return false; }
    return rec.base === file.name && Number(rec.size) === file.size &&
      Number(rec.mtime) === Number(file.lastModified || 0);
  }

  function idb(fn) {
    return new Promise(function (resolve, reject) {
      if (typeof indexedDB === 'undefined') { reject(new Error('IndexedDB 不可用')); return; }
      var req = indexedDB.open(DB, 1);
      req.onupgradeneeded = function () { req.result.createObjectStore(STORE); };
      req.onerror = function () { reject(req.error); };
      req.onsuccess = function () {
        try { resolve(fn(req.result)); } catch (e) { reject(e); }
      };
    });
  }

  function storeHandle(handle) {
    return idb(function (db) {
      var tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).put(handle, 'pending');
    });
  }

  function getHandle() {
    return idb(function (db) {
      return new Promise(function (resolve, reject) {
        var req = db.transaction(STORE, 'readonly').objectStore(STORE).get('pending');
        req.onsuccess = function () { resolve(req.result || null); };
        req.onerror = function () { reject(req.error); };
      });
    });
  }

  function el(id) { return document.getElementById(id); }

  function render() {
    var box = el('v2-pending');
    if (!box) { return; }
    var rec = load();
    if (!rec) { box.hidden = true; return; }
    box.hidden = false;
    var txt = el('v2-pending-text');
    if (txt) {
      var pct = rec.total > 0 ? Math.round((rec.received || 0) / rec.total * 100) : 0;
      txt.textContent = '未完成上传：' + (rec.name || rec.base) + '（已接收 ' + pct + '%）' +
        '——重新选择同一文件即可从断点继续' + (window.showOpenFilePicker ? '，或点「自动续传」' : '');
    }
  }

  /** 选择同一文件（自动续传）：优先 File System Access，其次聚焦上传输入框重选。 */
  function continueUpload() {
    var rec = load();
    if (!rec) { return; }
    if (window.showOpenFilePicker) {
      window.showOpenFilePicker({ multiple: false }).then(function (handles) {
        if (!handles || !handles.length) { return; }
        storeHandle(handles[0]).catch(function () { /* 忽略持久化失败 */ });
        return handles[0].getFile().then(function (file) {
          if (!sameFile(rec, file)) { throw new Error('所选文件与未完成上传不一致'); }
          if (window.v2uploadResume) { return window.v2uploadResume(file, rec.dir || ''); }
        });
      }).catch(function (e) {
        if (window.v2toast) { window.v2toast('warn', '续传失败：' + (e.message || e)); }
      });
      return;
    }
    var input = el('v2-upload-input');
    if (input) { input.click(); }
  }

  function ignore() {
    clear();
    if (window.v2toast) { window.v2toast('ok', '已忽略未完成上传（服务端会话按 TTL 自动过期）'); }
  }

  /** 刷新后若 Chromium 句柄仍在且权限未撤，自动续传。 */
  function autoResume() {
    if (!window.showOpenFilePicker) { return; }
    getHandle().then(function (handle) {
      if (!handle || !handle.queryPermission) { return; }
      return handle.queryPermission({ mode: 'read' }).then(function (perm) {
        if (perm !== 'granted') { return; }
        return handle.getFile().then(function (file) {
          var rec = load();
          if (sameFile(rec, file) && window.v2uploadResume) {
            return window.v2uploadResume(file, rec.dir || '');
          }
        });
      });
    }).catch(function () { /* 无句柄/权限/浏览器不支持均静默 */ });
  }

  var inited = false;
  function init() {
    if (inited) { return; }
    inited = true;
    var c = el('v2-pending-continue'); if (c) { c.addEventListener('click', continueUpload); }
    var i = el('v2-pending-ignore'); if (i) { i.addEventListener('click', ignore); }
    render();
    autoResume();
  }

  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading') { document.addEventListener('DOMContentLoaded', init); }
    else { init(); }
  }

  if (typeof window !== 'undefined') {
    window.v2pending = { init: init, save: save, load: load, clear: clear, render: render, sameFile: sameFile, continueUpload: continueUpload };
  }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { sameFile: sameFile };
  }
})();
