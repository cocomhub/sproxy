// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件写操作（P1b）：上传 / 新建目录 / 重命名 / 删除 / 批量删除。
// 请求签名复用 sclient/{sig,util,crypto}.js；服务端仍是最终校验方。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var auth = window.v2auth, f = window.v2files, ops = window.v2ops;
  var util = window.sclientUtil, cryptoLib = window.sclientCrypto, sig = window.sclientSig;
  if (!auth || !f || !ops || !util || !cryptoLib || !sig) { return; }

  function el(id) { return document.getElementById(id); }
  function enc(s) { return encodeURIComponent(s); }
  function view() { return window.v2filesView; }

  function toast(kind, msg) {
    var box = el('v2-toasts');
    if (!box) { return; }
    var t = document.createElement('div');
    t.className = 'toast ' + (kind || 'info');
    t.textContent = msg;
    box.appendChild(t);
    setTimeout(function () { t.remove(); }, 4000);
  }

  function signedFetch(method, path, bodyBytes, contentType, extra) {
    var creds = auth.loadCreds(window.sessionStorage);
    return auth.authHeaders(method, path, bodyBytes || null, creds, sig.signHeader)
      .then(function (h) {
        var headers = Object.assign({}, extra || {}, h);
        if (contentType) { headers['Content-Type'] = contentType; }
        return fetch(path, { method: method, headers: headers, body: bodyBytes || undefined, credentials: 'same-origin' });
      });
  }

  function readError(resp) {
    return resp.text().then(function (txt) {
      try { var j = JSON.parse(txt); return j.message || j.error || txt; } catch (e) { return txt || ('HTTP ' + resp.status); }
    });
  }

  function ensureOk(resp) {
    if (resp.ok) { return resp; }
    return readError(resp).then(function (msg) { throw new Error(msg); });
  }

  // ---------- 对话框 ----------
  var dlg = null;
  function buildDialog() {
    if (dlg) { return dlg; }
    var wrap = document.createElement('div');
    wrap.className = 'modal-backdrop';
    wrap.hidden = true;
    wrap.innerHTML = '<div class="modal" role="dialog" aria-modal="true">' +
      '<h3 id="v2-dlg-title"></h3><p id="v2-dlg-msg"></p>' +
      '<label class="field" id="v2-dlg-field">名称<input id="v2-dlg-input" autocomplete="off"></label>' +
      '<div class="modal-actions">' +
      '<button type="button" class="btn" data-dlg="cancel">取消</button>' +
      '<button type="button" class="btn btn-primary" data-dlg="ok">确定</button></div></div>';
    document.body.appendChild(wrap);
    dlg = {
      wrap: wrap,
      title: wrap.querySelector('#v2-dlg-title'),
      msg: wrap.querySelector('#v2-dlg-msg'),
      field: wrap.querySelector('#v2-dlg-field'),
      input: wrap.querySelector('#v2-dlg-input'),
      ok: wrap.querySelector('[data-dlg=ok]'),
      cancel: wrap.querySelector('[data-dlg=cancel]'),
      onOk: null
    };
    dlg.cancel.addEventListener('click', function () { closeDialog(); });
    wrap.addEventListener('click', function (e) { if (e.target === wrap) { closeDialog(); } });
    dlg.ok.addEventListener('click', submitDialog);
    dlg.input.addEventListener('keydown', function (e) { if (e.key === 'Enter') { submitDialog(); } });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && !wrap.hidden) { closeDialog(); }
    });
    return dlg;
  }

  function closeDialog() {
    if (!dlg) { return; }
    dlg.wrap.hidden = true;
    dlg.wrap.setAttribute('aria-hidden', 'true');
  }

  function submitDialog() {
    if (!dlg || !dlg.onOk) { return; }
    var cfg = dlg.cfg;
    var value = cfg.input ? dlg.input.value : '';
    if (cfg.input) {
      var v = ops.validateName(value);
      if (!v.ok) { toast('warn', v.reason); return; }
      value = v.name;
    }
    var cb = dlg.onOk;
    closeDialog();
    cb(value);
  }

  function openDialog(cfg) {
    buildDialog();
    dlg.cfg = cfg;
    dlg.title.textContent = cfg.title;
    dlg.msg.textContent = cfg.message || '';
    dlg.field.hidden = !cfg.input;
    dlg.input.value = cfg.value || '';
    dlg.ok.textContent = cfg.okText || '确定';
    dlg.ok.className = 'btn ' + (cfg.danger ? 'btn-danger' : 'btn-primary');
    dlg.onOk = cfg.onOk;
    dlg.wrap.hidden = false;
    dlg.wrap.setAttribute('aria-hidden', 'false');
    if (cfg.input) { dlg.input.focus(); dlg.input.select(); } else { dlg.ok.focus(); }
  }

  // ---------- 操作 ----------
  function run(promise, okMsg) {
    return promise.then(function () {
      toast('ok', okMsg);
      if (view()) { view().load(); }
    }).catch(function (err) {
      toast('bad', '操作失败：' + (err && err.message ? err.message : '网络错误'));
    });
  }

  function uploadFiles(files) {
    var sub = view() ? view().subdir() : '';
    var list = Array.prototype.slice.call(files || []);
    if (!list.length) { return; }
    var chain = Promise.resolve();
    list.forEach(function (file) {
      chain = chain.then(function () {
        return file.arrayBuffer().then(function (buf) {
          var bytes = new Uint8Array(buf);
          return cryptoLib.sha256Hex(bytes).then(function (sum) {
            var rel = f.joinSubdir(sub, file.name);
            var mp = util.buildMultipart({}, { name: 'file', filename: rel, contentType: 'application/octet-stream', bytes: bytes });
            return signedFetch('POST', '/upload', mp.body, mp.contentType, ops.uploadHeaders(rel, sum, file.lastModified))
              .then(ensureOk)
              .then(function (resp) { return resp.json().catch(function () { return {}; }); })
              .then(function (body) {
                if (body && body.success === false) { throw new Error(body.message || '上传失败'); }
                toast('ok', '已上传 ' + file.name);
              });
          });
        });
      });
    });
    chain.then(function () { if (view()) { view().load(); } })
      .catch(function (err) { toast('bad', '上传失败：' + (err && err.message ? err.message : '网络错误')); });
  }

  function mkdir() {
    openDialog({
      title: '新建目录', message: '在当前目录下创建；名称不能包含 / \\ : * ? " < > |',
      input: true, value: '', okText: '创建',
      onOk: function (name) {
        var sub = view() ? view().subdir() : '';
        var rel = f.joinSubdir(sub, name);
        run(signedFetch('POST', '/mkdir?dirname=' + enc(rel), null, null, {}).then(ensureOk), '已创建目录 ' + name);
      }
    });
  }

  function doDelete(name, isDir, checksum) {
    var sub = view() ? view().subdir() : '';
    var rel = f.joinSubdir(sub, name);
    openDialog({
      title: isDir ? '删除目录？' : '删除文件？',
      message: (isDir ? '目录及其内容将被删除' : '文件将从存储中删除') + '：' + rel,
      danger: true, okText: '删除',
      onOk: function () {
        var path = isDir ? '/rmdir?dirname=' + enc(rel) + '&force=true' : '/delete?filename=' + enc(rel);
        run(signedFetch('POST', path, null, null, ops.checksumHeader(checksum)).then(ensureOk), '已删除 ' + name);
      }
    });
  }

  function doRename(name, checksum) {
    var sub = view() ? view().subdir() : '';
    var from = f.joinSubdir(sub, name);
    openDialog({
      title: '重命名', message: '当前：' + from, input: true, value: name, okText: '保存',
      onOk: function (newName) {
        var to = f.joinSubdir(sub, newName);
        run(signedFetch('POST', '/rename?from=' + enc(from) + '&to=' + enc(to), null, null, ops.checksumHeader(checksum)).then(ensureOk), '已重命名为 ' + newName);
      }
    });
  }

  function init() {
    var input = el('v2-upload-input');
    if (input) {
      input.addEventListener('change', function () {
        uploadFiles(input.files);
        input.value = '';
      });
    }
    var mk = el('v2-mkdir');
    if (mk) { mk.addEventListener('click', mkdir); }

    var batch = el('v2-batch-delete');
    if (batch) {
      batch.addEventListener('click', function () {
        var names = view() ? view().selected() : [];
        if (!names.length) { return; }
        openDialog({
          title: '删除所选 ' + names.length + ' 项？',
          message: '删除后不可恢复；目录将连同内容一并删除。',
          danger: true, okText: '删除',
          onOk: function () {
            var sub = view() ? view().subdir() : '';
            var chain = Promise.resolve();
            names.forEach(function (name) {
              chain = chain.then(function () {
                var info = view() && view().rowInfo(name);
                var rel = f.joinSubdir(sub, name);
                var path = info && info.isDir ? '/rmdir?dirname=' + enc(rel) + '&force=true' : '/delete?filename=' + enc(rel);
                return signedFetch('POST', path, null, null, ops.checksumHeader(info ? info.checksum : '')).then(ensureOk);
              });
            });
            run(chain, '已删除 ' + names.length + ' 项');
          }
        });
      });
    }

    var body = el('v2-file-body');
    if (body) {
      body.addEventListener('click', function (e) {
        var b = e.target.closest('[data-action]');
        if (!b) { return; }
        var name = b.getAttribute('data-file');
        var info = view() && view().rowInfo(name);
        if (!info) { return; }
        if (b.getAttribute('data-action') === 'delete') { doDelete(name, info.isDir, info.checksum); }
        if (b.getAttribute('data-action') === 'rename') { doRename(name, info.checksum); }
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
