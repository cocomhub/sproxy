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
  function T(k) { return (window.v2i18n ? window.v2i18n.t(k) : k); }

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
      // 重命名/移动允许相对路径（a/b/new）；建目录仍是单段名称。
      var v = cfg.allowPath ? ops.validateRelativePath(value) : ops.validateName(value);
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

  function uploadChunkedFile(file, sub, vol) {
    var item = uploadItem(file);
    if (!item) { return Promise.resolve(); }
    var total = file.size;
    item.pause.addEventListener('click', function () {
      if (item.cancelled) { run(); return; }
      item.cancelled = true;
      item.pause.textContent = T('toast.paused');
    });
    item.cancel.addEventListener('click', function () {
      item.cancelled = true;
      item.row.remove();
      if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
    });
    var run = function () {
      item.cancelled = false;
      item.pause.textContent = T('dlg.cancel') === 'Cancel' ? 'Pause' : '暂停';
      item.started = Date.now();
      return chunked.uploadChunked(file, {
        api: api, util: util, cryptoLib: cryptoLib, shaCtor: window.sclientSha256, join: f.joinSubdir
      }, {
        subdir: sub,
        volume: vol || '',
        onProgress: progressHandler(item, total),
        isCancelled: function () { return item.cancelled; }
      }).then(function (res) {
        item.row.remove();
        if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
        toast('ok', res && res.skipped ? (T('toast.exists') + file.name) : (T('toast.uploaded') + file.name));
        if (view()) { view().load(); }
      }).catch(function (err) {
        if (err && err.code === 'E_CANCELLED') {
          item.pause.textContent = T('toast.resume');
          item.info.textContent = '已暂停 · ' + (item.info.textContent.split(' · ')[0] || '');
          return;
        }
        item.row.remove();
        if (!el('v2-uploads').children.length) { el('v2-uploads').hidden = true; }
        toast('bad', T('toast.uploadFailed') + (err && err.message ? err.message : T('toast.network')));
      });
    };
    return run();
  }

  /** 供「未完成上传」续传复用：按指定目录上传（分块走既有断点逻辑）。 */
  function resumeUpload(file, dir) {
    return uploadChunkedFile(file, dir || '');
  }
  window.v2uploadResume = resumeUpload;

  function uploadSimpleFile(file, sub, vol) {
    return file.arrayBuffer().then(function (buf) {
      var bytes = new Uint8Array(buf);
      return cryptoLib.sha256Hex(bytes).then(function (sum) {
        var rel = relOf(file.name);
        var mp = util.buildMultipart({}, { name: 'file', filename: rel, contentType: 'application/octet-stream', bytes: bytes });
        var path = '/upload' + (vol ? '?volume=' + encodeURIComponent(vol) : '');
        return api.request('POST', path, { body: mp.body, contentType: mp.contentType, headers: ops.uploadHeaders(rel, sum, file.lastModified) })
          .then(api.ensureOk)
          .then(function (resp) { return resp.json().catch(function () { return {}; }); })
          .then(function (body) {
            if (body && body.success === false) { throw new Error(body.message || '上传失败'); }
            toast('ok', '已上传 ' + file.name);
          });
      });
    });
  }

  /**
   * 拖拽上传接线（旧页有，v2 此前缺失）。
   *
   * dragenter/dragleave 会因**子元素**反复触发 ⇒ 用计数抵消；只接受带 Files 的拖拽
   * （拖选文字/链接不应触发上传）。drop 后按 `uploadFiles` 同一条路径上传。
   */
  function initDropUpload() {
    var du = window.v2dropUpload;
    var zone = el('v2-drop-zone');
    var hint = el('v2-drop-hint');
    if (!du || !zone) { return; }
    var depth = 0;
    var show = function (on) {
      zone.hidden = !on;
      if (on && hint) {
        hint.textContent = du.hintText(view() ? view().subdir() : '', view() ? view().volume() : '');
      }
    };
    var stop = function (e) { e.preventDefault(); e.stopPropagation(); };
    document.addEventListener('dragenter', function (e) {
      if (!du.acceptsDrag(e.dataTransfer && e.dataTransfer.types)) { return; }
      stop(e);
      depth = du.bump(depth, 1);
      if (depth === 1) { show(true); }
    });
    document.addEventListener('dragover', function (e) {
      if (!du.acceptsDrag(e.dataTransfer && e.dataTransfer.types)) { return; }
      stop(e);
    });
    document.addEventListener('dragleave', function (e) {
      if (!du.acceptsDrag(e.dataTransfer && e.dataTransfer.types)) { return; }
      stop(e);
      depth = du.bump(depth, -1);
      if (depth === 0) { show(false); }
    });
    document.addEventListener('drop', function (e) {
      if (!du.acceptsDrag(e.dataTransfer && e.dataTransfer.types)) { return; }
      stop(e);
      depth = 0;
      show(false);
      var files = (e.dataTransfer && e.dataTransfer.files) || [];
      if (files.length) { uploadFiles(files); }
    });
  }

  /**
   * 上传目标卷：卷过滤有值 ⇒ 用它；否则用户配置的「默认上传卷」；都没有 ⇒ 空 = 服务端
   * 自动路由。配置卷可能已不存在 ⇒ 由服务端按未知卷报错（**不静默改道**，避免传错卷）。
   */
  function uploadTargetVolume() {
    var picked = view() && view().volume ? view().volume() : '';
    if (picked) { return picked; }
    return (view() && view().defaultUploadVolume) ? view().defaultUploadVolume() : '';
  }

  function uploadFiles(files) {
    var sub = view() ? view().subdir() : '';
    var vol = uploadTargetVolume();
    var list = Array.prototype.slice.call(files || []);
    if (!list.length) { return; }
    var chain = Promise.resolve();
    list.forEach(function (file) {
      chain = chain.then(function () {
        if (chunked && file.size > chunked.SIMPLE_LIMIT) { return uploadChunkedFile(file, sub, vol); }
        return uploadSimpleFile(file, sub, vol);
      });
    });
    chain.then(function () { if (view()) { view().load(); } })
      .catch(function (err) { toast('bad', T('toast.uploadFailed') + (err && err.message ? err.message : T('toast.network'))); });
  }

  function mkdir() {
    openDialog({
      title: T('dlg.mkdir.title'), message: T('dlg.mkdir.msg'),
      input: true, value: '', okText: T('dlg.create'),
      onOk: function (name) {
        var sub = view() ? view().subdir() : '';
        var rel = relOf(name);
        run(api.json('POST', '/mkdir?dirname=' + enc(rel)), '已创建目录 ' + name);
      }
    });
  }

  function doDelete(name, isDir, checksum) {
    var sub = view() ? view().subdir() : '';
    var rel = relOf(name);
    openDialog({
      title: isDir ? T('dlg.delDir.title') : T('dlg.delFile.title'),
      message: (isDir ? T('dlg.delDir.msg') : T('dlg.delFile.msg')) + rel,
      danger: true, okText: T('dlg.delete'),
      onOk: function () {
        // 页面按用户配置（设置 → 删除默认方式）**显式**传 soft：
        // 服务端 API 默认语义保持不变（CLI/脚本零回归），配置只决定页面默认行为。
        var path = isDir ? '/rmdir?dirname=' + enc(rel) + '&force=true'
          : '/delete?filename=' + enc(rel) + '&soft=' + (deleteMode() === 'hard' ? 'false' : 'true');
        run(api.json('POST', path, { headers: ops.checksumHeader(checksum) }), T('toast.deleted') + name);
      }
    });
  }

  function doRename(name, checksum, isDir) {
    var from = relOf(name);
    openDialog({
      title: T('dlg.rename.title'),
      message: T('dlg.rename.current') + from + String.fromCharCode(10) + T('dlg.rename.hint'),
      input: true, value: name, okText: T('dlg.save'), allowPath: true,
      onOk: function (newName) {
        // 目标路径：与源同目录（搜索态源是完整路径，必须用其目录前缀，而不是当前 subdir）。
        // 输入里带 "/" 视为**移动到该相对路径**（目录自动创建），否则是同目录改名。
        var dir = from.indexOf('/') >= 0 ? from.slice(0, from.lastIndexOf('/')) : '';
        var to = String(newName || '').indexOf('/') >= 0 ? f.joinSubdir('', newName) : f.joinSubdir(dir, newName);
        // 目录没有单一 checksum：不带校验头（服务端按 Lstat 分派到目录重命名）。
        var opts = isDir ? {} : { headers: ops.checksumHeader(checksum) };
        run(api.json('POST', '/rename?from=' + enc(from) + '&to=' + enc(to), opts), T('toast.renamed') + ' ' + newName);
      }
    });
  }

  /** 行名 → 相对路径（搜索态 name 已是完整路径，见 v2files.resolveRel）。 */
  /** 当前「页面默认删除方式」：soft（缺省）/ hard，来自 /api/ui/prefs（设置页写入）。 */
  var deleteModeCache = 'soft';
  function deleteMode() { return deleteModeCache === 'hard' ? 'hard' : 'soft'; }

  /** 读取页面配置（失败 = soft 安全默认）。 */
  function loadDeleteMode() {
    if (!api) { return Promise.resolve(); }
    return api.json('GET', '/api/ui/prefs')
      .then(function (body) {
        var d = body && body.prefs && body.prefs['delete'];
        deleteModeCache = (d && d.default_mode === 'hard') ? 'hard' : 'soft';
      })
      .catch(function () { deleteModeCache = 'soft'; });
  }
  window.v2deleteMode = { mode: deleteMode, reload: loadDeleteMode };

  function relOf(name) {
    var sub = view() ? view().subdir() : '';
    var search = !!(view() && view().search && view().search());
    return f.resolveRel(sub, name, search) || name;
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
    if (!items.length) { toast('warn', '请选择文件（目录请用行内「重命名」，批量仅对文件生效）'); return; }
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
      }).catch(function (e) { toast('bad', T('toast.batchRenameFailed') + (e.message || e)); });
    });
    var input = wrap.querySelector('#v2-br-prefix');
    if (input) { input.focus(); }
  }

  /** 目录打包下载：GET /api/archive-dir?dirname=… 流式 tar.gz。 */
  function archiveDir(name) {
    var rel = relOf(name);
    var url = '/api/archive-dir?dirname=' + enc(rel);
    toast('info', '正在打包目录…');
    api.request('GET', url).then(api.ensureOk).then(function (resp) { return resp.blob(); })
      .then(function (blob) {
        var objURL = URL.createObjectURL(blob);
        var a = document.createElement('a');
        a.href = objURL;
        a.download = name + '.tar.gz';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(function () { URL.revokeObjectURL(objURL); }, 2000);
        toast('ok', '已开始下载 ' + name + '.tar.gz');
      })
      .catch(function (e) { toast('bad', '打包失败：' + (e.message || e)); });
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
    initDropUpload();
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
          title: T('dlg.delSel.title'),
          message: T('dlg.delSel.msg'),
          danger: true, okText: T('dlg.delete'),
          onOk: function () {
            var sub = view() ? view().subdir() : '';
            // 文件走**批量族**（一次请求，服务端逐条聚合）：软/硬由服务端按用户配置决定
            // （此前逐个 /delete 串行，N 个文件 = N 次请求；且批量族此前无软删入口）。
            var files = [];
            var dirs = [];
            names.forEach(function (name) {
              var info = view() && view().rowInfo(name);
              var rel = relOf(name);
              if (info && info.isDir) { dirs.push(rel); return; }
              files.push({ filename: rel, checksum: info ? info.checksum : '' });
            });
            var chain = Promise.resolve();
            if (dirs.length) {
              // 目录必须逐个 /rmdir（无批量族）；不需要 checksum 头。
              dirs.forEach(function (rel) {
                chain = chain.then(function () {
                  return api.json('POST', '/rmdir?dirname=' + enc(rel) + '&force=true', {});
                });
              });
            }
            if (files.length) {
              chain = chain.then(function () {
                // 批量族同样按页面配置**显式**传 soft（与单条一致）。
                return api.json('POST', '/api/batch/delete', {
                  body: new TextEncoder().encode(JSON.stringify({ soft: deleteMode() !== 'hard', files: files })),
                  contentType: 'application/json'
                }).then(function (res) {
                  var results = (res && res.results) || [];
                  var okN = results.filter(function (r) { return r.success; }).length;
                  if (okN !== results.length) {
                    toast('warn', '已删除 ' + okN + '/' + results.length + ' 项（部分失败）');
                  }
                });
              });
            }
            run(chain, T('toast.deleted') + names.length);
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
        if (act === 'archive') { archiveDir(name); return; }
        if (act === 'delete') { doDelete(name, info.isDir, info.checksum); }
        if (act === 'rename') { doRename(name, info.checksum, info.isDir); }
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
