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

  var api = window.v2api;

  window.v2toast = toast;

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

  var chunked = window.v2chunked;

  function uploadItem(file) {
    var box = el('v2-uploads');
    if (!box) { return null; }
    var row = document.createElement('div');
    row.className = 'up-row';
    var name = document.createElement('div');
    name.className = 'up-name';
    name.textContent = file.name;
    var bar = document.createElement('div');
    bar.className = 'bar';
    var fill = document.createElement('i');
    fill.style.width = '0%';
    bar.appendChild(fill);
    var info = document.createElement('div');
    info.className = 'up-info';
    info.textContent = '0%';
    var acts = document.createElement('div');
    acts.className = 'up-acts';
    var pause = document.createElement('button');
    pause.type = 'button'; pause.className = 'btn btn-sm btn-ghost'; pause.textContent = '暂停';
    var cancel = document.createElement('button');
    cancel.type = 'button'; cancel.className = 'btn btn-sm btn-ghost'; cancel.textContent = '取消';
    acts.appendChild(pause); acts.appendChild(cancel);
    row.appendChild(name); row.appendChild(bar); row.appendChild(info); row.appendChild(acts);
    box.hidden = false;
    box.appendChild(row);
    return { row: row, fill: fill, info: info, pause: pause, cancel: cancel, cancelled: false, started: Date.now() };
  }

  function fmtSpeed(bytes, ms) {
    if (!ms) { return '—'; }
    var bps = bytes / (ms / 1000);
    if (bps >= 1024 * 1024) { return (bps / 1024 / 1024).toFixed(1) + ' MB/s'; }
    if (bps >= 1024) { return (bps / 1024).toFixed(0) + ' KB/s'; }
    return bps.toFixed(0) + ' B/s';
  }

  function progressHandler(item, total) {
    return function (loaded) {
      var pct = total ? Math.min(100, Math.round(loaded / total * 100)) : 0;
      item.fill.style.width = pct + '%';
      var elapsed = Date.now() - item.started;
      var speed = fmtSpeed(loaded, elapsed);
      var remain = loaded > 0 ? Math.max(0, Math.round((total - loaded) / (loaded / (elapsed / 1000)))) : 0;
      item.info.textContent = pct + '% · ' + speed + (remain ? ' · 剩余 ' + remain + 's' : '');
    };
  }

  function uploadChunkedFile(file, sub) {
    var item = uploadItem(file);
    if (!item) { return Promise.resolve(); }
    var total = file.size;
    item.pause.addEventListener('click', function () {
      if (item.cancelled) { run(); return; }
      item.cancelled = true;
      item.pause.textContent = '已暂停';
    });
    item.cancel.addEventListener('click', function () {
      item.cancelled = true;
      item.row.remove();
      if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
    });
    var run = function () {
      item.cancelled = false;
      item.pause.textContent = '暂停';
      item.started = Date.now();
      return chunked.uploadChunked(file, {
        api: api, util: util, cryptoLib: cryptoLib, shaCtor: window.sclientSha256, join: f.joinSubdir
      }, {
        subdir: sub,
        onProgress: progressHandler(item, total),
        isCancelled: function () { return item.cancelled; }
      }).then(function (res) {
        item.row.remove();
        if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
        toast('ok', res && res.skipped ? ('已存在，跳过 ' + file.name) : ('已上传 ' + file.name));
        if (view()) { view().load(); }
      }).catch(function (err) {
        if (err && err.code === 'E_CANCELLED') {
          item.pause.textContent = '继续';
          item.info.textContent = '已暂停 · ' + (item.info.textContent.split(' · ')[0] || '');
          return;
        }
        item.row.remove();
        if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
        toast('bad', '上传失败：' + (err && err.message ? err.message : '网络错误'));
      });
    };
    return run();
  }

  /** 供「未完成上传」续传复用：按指定目录上传（分块走既有断点逻辑）。 */
  function resumeUpload(file, dir) {
    return uploadChunkedFile(file, dir || '');
  }
  window.v2uploadResume = resumeUpload;

  function uploadSimpleFile(file, sub) {
    return file.arrayBuffer().then(function (buf) {
      var bytes = new Uint8Array(buf);
      return cryptoLib.sha256Hex(bytes).then(function (sum) {
        var rel = f.joinSubdir(sub, file.name);
        var mp = util.buildMultipart({}, { name: 'file', filename: rel, contentType: 'application/octet-stream', bytes: bytes });
        return api.request('POST', '/upload', { body: mp.body, contentType: mp.contentType, headers: ops.uploadHeaders(rel, sum, file.lastModified) })
          .then(api.ensureOk)
          .then(function (resp) { return resp.json().catch(function () { return {}; }); })
          .then(function (body) {
            if (body && body.success === false) { throw new Error(body.message || '上传失败'); }
            toast('ok', '已上传 ' + file.name);
          });
      });
    });
  }

  function uploadFiles(files) {
    var sub = view() ? view().subdir() : '';
    var list = Array.prototype.slice.call(files || []);
    if (!list.length) { return; }
    var chain = Promise.resolve();
    list.forEach(function (file) {
      chain = chain.then(function () {
        if (chunked && file.size > chunked.SIMPLE_LIMIT) { return uploadChunkedFile(file, sub); }
        return uploadSimpleFile(file, sub);
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
        run(api.json('POST', '/mkdir?dirname=' + enc(rel)), '已创建目录 ' + name);
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
        var path = isDir ? '/rmdir?dirname=' + enc(rel) + '&force=true' : '/delete?filename=' + enc(rel) + '&soft=true';
        run(api.json('POST', path, { headers: ops.checksumHeader(checksum) }), '已删除 ' + name);
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
        run(api.json('POST', '/rename?from=' + enc(from) + '&to=' + enc(to), { headers: ops.checksumHeader(checksum) }), '已重命名为 ' + newName);
      }
    });
  }

  function relOf(name) {
    var sub = view() ? view().subdir() : '';
    var j = window.v2files && window.v2files.joinSubdir ? window.v2files.joinSubdir(sub, name) : name;
    return j || name;
  }

  function selectedFiles() {
    var names = view() ? view().selected() : [];
    return names.map(function (name) {
      var info = view() && view().file ? view().file(name) : null;
      return { name: name, rel: relOf(name), info: info || {} };
    });
  }

  function batchRenameSelected() {
    var items = selectedFiles().filter(function (it) { return !it.info || !it.info.isDir; });
    if (!items.length) { toast('warn', '请选择文件（目录不支持批量重命名）'); return; }
    var wrap = document.createElement('div');
    wrap.className = 'modal-backdrop';
    wrap.innerHTML = '<div class="modal" role="dialog" aria-modal="true">' +
      '<h3>批量重命名 ' + items.length + ' 项</h3>' +
      '<p class="muted">在原名基础上添加前缀/后缀（不改目录）</p>' +
      '<label class="field">前缀<input id="v2-br-prefix" autocomplete="off"></label>' +
      '<label class="field">后缀<input id="v2-br-suffix" autocomplete="off"></label>' +
      '<div class="modal-actions"><button type="button" class="btn" data-br="cancel">取消</button>' +
      '<button type="button" class="btn btn-primary" data-br="ok">重命名</button></div></div>';
    document.body.appendChild(wrap);
    var close = function () { wrap.remove(); };
    wrap.addEventListener('click', function (e) { if (e.target === wrap) { close(); } });
    wrap.querySelector('[data-br=cancel]').addEventListener('click', close);
    wrap.querySelector('[data-br=ok]').addEventListener('click', function () {
      var pre = wrap.querySelector('#v2-br-prefix').value;
      var suf = wrap.querySelector('#v2-br-suffix').value;
      if (!pre && !suf) { toast('warn', '请填写前缀或后缀'); return; }
      var ops = items.map(function (it) {
        return {
          from: it.rel,
          to: relOf(pre + it.name + suf),
          checksum: (it.info && it.info.checksum) || ''
        };
      });
      close();
      api.json('POST', '/api/batch/rename', {
        body: new TextEncoder().encode(JSON.stringify({ operations: ops })),
        contentType: 'application/json'
      }).then(function (body) {
        var results = (body && body.results) || [];
        var okN = results.filter(function (r) { return r.success; }).length;
        toast(okN === results.length ? 'ok' : 'warn', '重命名成功 ' + okN + '/' + results.length);
        if (view()) { view().load(); }
      }).catch(function (e) { toast('bad', '批量重命名失败：' + (e.message || e)); });
    });
    var input = wrap.querySelector('#v2-br-prefix');
    if (input) { input.focus(); }
  }

  function archiveSelected() {
    var items = selectedFiles();
    if (!items.length) { return; }
    var files = items.map(function (it) { return it.rel; });
    toast('info', '正在打包 ' + files.length + ' 项…');
    api.request('POST', '/api/archive', {
      body: new TextEncoder().encode(JSON.stringify({ files: files })),
      contentType: 'application/json'
    }).then(api.ensureOk).then(function (resp) { return resp.blob(); })
      .then(function (blob) {
        var url = URL.createObjectURL(blob);
        var a = document.createElement('a');
        a.href = url;
        var d = new Date();
        a.download = 'archive-' + d.toISOString().slice(0, 10) + '.tar.gz';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
        toast('ok', '已开始下载归档（' + files.length + ' 项）');
      })
      .catch(function (e) { toast('bad', '打包失败：' + (e.message || e)); });
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

    var br = el('v2-batch-rename'); if (br) { br.addEventListener('click', batchRenameSelected); }
    var ba = el('v2-batch-archive'); if (ba) { ba.addEventListener('click', archiveSelected); }
    var batch = el('v2-batch-delete');
    if (batch) {
      batch.addEventListener('click', function () {
        var names = view() ? view().selected() : [];
        if (!names.length) { return; }
        openDialog({
          title: '删除所选 ' + names.length + ' 项？',
          message: '文件会移入回收站（可恢复）；目录将直接删除并连同内容。',
          danger: true, okText: '删除',
          onOk: function () {
            var sub = view() ? view().subdir() : '';
            var chain = Promise.resolve();
            names.forEach(function (name) {
              chain = chain.then(function () {
                var info = view() && view().rowInfo(name);
                var rel = f.joinSubdir(sub, name);
                var path = info && info.isDir ? '/rmdir?dirname=' + enc(rel) + '&force=true' : '/delete?filename=' + enc(rel) + '&soft=true';
                return api.json('POST', path, { headers: ops.checksumHeader(info ? info.checksum : '') });
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
        var act = b.getAttribute('data-action');
        if (act === 'detail' && window.v2inspector) { window.v2inspector.open(name); return; }
        if (act === 'download' && window.v2preview) { window.v2preview.download(name); return; }
        if (act === 'delete') { doDelete(name, info.isDir, info.checksum); }
        if (act === 'rename') { doRename(name, info.checksum); }
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
