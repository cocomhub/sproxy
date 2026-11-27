// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 文件写操作（P1b）：上传 / 新建目录 / 重命名 / 删除 / 批量删除。
// 请求签名复用 sclient/{sig,util,crypto}.js；服务端仍是最终校验方。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var auth = window.v2auth, f = window.v2files, ops = window.v2ops;
  /** 当前「页面默认删除方式」：soft（缺省）/ hard，来自 /api/ui/prefs（设置页写入）。 */
  var deleteModeCache = 'soft';
  /** 「跳过删除二次确认」：true 时危险操作直接执行（用户显式开启；缺省 false = 弹确认）。 */
  var skipConfirmCache = false;
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

  /**
   * 危险操作确认入口：`skip_confirm` 开启时**直接执行**（用户显式一次性授权），
   * 否则照常弹确认框并展示 `${cfg.message}`（实际破坏范围）。
   * 只对 `danger: true` 生效；普通输入框（新建目录/重命名）**始终**需要交互。
   */
  function confirmDanger(cfg) {
    if (cfg && cfg.danger && skipConfirmCache && typeof cfg.onOk === 'function') {
      cfg.onOk(cfg.value || '');
      return;
    }
    // **无输入框的确认统一走 window.v2confirm**（与云端任务的破坏性动作同一实现）：
    // 此前本文件另有一套纯确认渲染，两份实现各自演化（提示文案/danger 样式/Esc 行为
    // 都要求一致）。带输入框的（新建目录/重命名）仍用本文件的 openDialog——那是另一种能力。
    if (cfg && !cfg.input && typeof window.v2confirm === 'function') {
      window.v2confirm({
        title: cfg.title, message: cfg.message, okText: cfg.okText, danger: !!cfg.danger
      }, function () { if (typeof cfg.onOk === 'function') { cfg.onOk(cfg.value || ''); } });
      return;
    }
    openDialog(cfg);
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

  /**
   * 建一行上传进度。
   *
   * 状态机（此前缺失）：
   *   排队中（queued）→ 上传中（running）→ 完成（移除）/ 暂停（paused，可继续）/ 失败（failed，可重试/关闭）。
   * **失败不再直接删行**：以前失败只弹 toast 并移除行，用户看不出**哪个**文件失败、也无法原地重试。
   */
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
    row.setAttribute('data-state', 'queued');
    row.setAttribute('data-file', file.name);
    box.hidden = false;
    box.appendChild(row);
    var item = {
      row: row, fill: fill, info: info, pause: pause, cancel: cancel,
      cancelled: false, started: Date.now(), state: 'queued'
    };
    item.info.textContent = '排队中…';
    pause.hidden = true;   // 排队中不可暂停，进入上传后再显示
    return item;
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

  function uploadChunkedFile(file, sub, vol, preItem) {
    // preItem：调用方（多文件串行队列）已建好「排队中」行；单文件/续传路径自建。
    var item = preItem || uploadItem(file);
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
      pruneUploadsBox();
      sumUploads();
    });
    var run = function () {
      item.cancelled = false;
      item.state = 'running';
      item.row.setAttribute('data-state', 'running');
      item.pause.hidden = false;
      item.pause.textContent = '暂停';
      item.cancel.textContent = '取消';
      item.started = Date.now();
      return chunked.uploadChunked(file, {
        api: api, util: util, cryptoLib: cryptoLib, shaCtor: window.sclientSha256, join: f.joinSubdir
      }, {
        subdir: sub,
        volume: vol || '',
        onProgress: progressHandler(item, total),
        isCancelled: function () { return item.cancelled; }
      }).then(function (res) {
        item.state = 'done';
        item.row.remove();
        pruneUploadsBox();
        sumUploads();
        toast('ok', res && res.skipped ? (T('toast.exists') + file.name) : (T('toast.uploaded') + file.name));
        if (view()) { view().load(); }
      }).catch(function (err) {
        if (err && err.code === 'E_CANCELLED') {
          item.state = 'paused';
          item.row.setAttribute('data-state', 'paused');
          item.pause.textContent = '继续';
          item.info.textContent = '已暂停 · ' + (item.info.textContent.split(' · ')[0] || '');
          sumUploads();
          return;
        }
        markFailed(item, file, err, function () { run(); });
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
    /** 当前视图是否允许上传（「全部卷」聚合视图不允许）。 */
    var allowed = function () { return f.canUploadTo(view() ? view().volume() : ''); };
    document.addEventListener('dragenter', function (e) {
      if (!du.acceptsDrag(e.dataTransfer && e.dataTransfer.types)) { return; }
      // 不允许上传时不显示投放层（否则会承诺一个做不到的动作）。
      if (!allowed()) { return; }
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
      if (!allowed()) {
        if (window.v2toast) {
          window.v2toast('warn', '请先选择具体卷再用上传（「全部卷」是多卷聚合视图，无法确定上传目标）');
        }
        return;
      }
      depth = 0;
      show(false);
      var files = (e.dataTransfer && e.dataTransfer.files) || [];
      if (files.length) { uploadFiles(files); }
    });
  }

  /**
   * 把一行标成「失败」并挂上重试。
   *
   * 为什么抽出来：分块路径与小文件路径**都必须**有失败态 —— 早期只有分块路径处理，
   * 小文件失败时行永远停在「排队中」（既无原因也无重试），是本次审查发现的真实缺陷。
   *
   * @param {object} item 行对象
   * @param {File} file 源文件
   * @param {Error} err 失败原因
   * @param {function} retry 重试回调（重新发起该文件的上传）
   */
  function markFailed(item, file, err, retry) {
    item.state = 'failed';
    item.row.setAttribute('data-state', 'failed');
    item.info.textContent = '失败：' + ((err && err.message) || T('toast.network'));
    item.pause.hidden = true;
    item.cancel.textContent = '关闭';
    if (!item.retry) {
      item.retry = document.createElement('button');
      item.retry.type = 'button';
      item.retry.className = 'btn btn-sm';
      item.retry.textContent = '重试';
      item.retry.addEventListener('click', function () {
        item.retry.remove(); item.retry = null;
        item.fill.style.width = '0%';
        retry();
        sumUploads();
      });
      item.cancel.parentNode.insertBefore(item.retry, item.cancel);
    }
    sumUploads();
    toast('bad', T('toast.uploadFailed') + file.name);
  }

  /** 队列为空时隐藏容器（多点复用，避免各处重复判断）。 */
  function pruneUploadsBox() {
    var box = el('v2-uploads');
    // 按 `.up-row` 计数而非 children.length：容器里还有常驻的汇总节点（#v2-uploads-sum），
    // 用 children.length 会导致「没有上传行但容器永不隐藏」（实测）。
    if (box && !box.querySelectorAll('.up-row').length) {
      box.hidden = true;
      var sum = el('v2-uploads-sum');
      if (sum) { sum.textContent = ''; }
    }
  }

  /**
   * 顶部汇总：`已完成 x/y · 失败 z`。此前只有逐行进度，多文件时看不出整体还剩多少。
   * 只在容器有 `#v2-uploads-sum` 时渲染（向后兼容）。
   */
  function sumUploads() {
    var box = el('v2-uploads');
    var sum = el('v2-uploads-sum');
    if (!box || !sum) { return; }
    var rows = Array.prototype.slice.call(box.querySelectorAll('.up-row'));
    if (!rows.length) { sum.textContent = ''; return; }
    var count = function (st) {
      return rows.filter(function (r) { return r.getAttribute('data-state') === st; }).length;
    };
    var queued = count('queued');
    var running = count('running');
    var failed = count('failed');
    var paused = count('paused');
    // 文案必须**只统计真在上传的**：此前把 failed/paused/queued 都算进「上传中」，
    // 与旁边的「失败 N」自相矛盾（审查发现的措辞缺陷）。
    var parts = [];
    if (running) { parts.push('上传中 ' + running); }
    if (queued) { parts.push('排队 ' + queued); }
    if (paused) { parts.push('暂停 ' + paused); }
    if (failed) { parts.push('失败 ' + failed); }
    if (!parts.length) { parts.push('共 ' + rows.length + ' 项'); }
    sum.textContent = parts.join(' · ');
  }

  function uploadFiles(files) {
    var sub = view() ? view().subdir() : '';
    // 兜底：必须有**具体卷**（「全部卷」是多卷聚合视图，上传目标不可预期）。
    // UI 已禁用入口，这里再挡一次——拖拽/脚本/旧快捷键都可能绕过按钮状态。
    var vol = view() && view().volume ? view().volume() : '';
    if (!f.canUploadTo(vol)) {
      toast('warn', '请先选择具体卷再用上传（「全部卷」是多卷聚合视图，无法确定上传目标）');
      return;
    }
    var list = Array.prototype.slice.call(files || []);
    if (!list.length) { return; }
    // 串行上传（避免并发抢带宽）：先全部建行显示「排队中」，轮到谁谁才开始。
    var items = list.map(function (file) {
      return { file: file, item: uploadItem(file) };
    });
    sumUploads();
    /** 跑一个文件（分块 / 小文件两条路径，失败都被收敛到行上的 failed 态）。 */
    var attempt = function (pair) {
      var file = pair.file;
      var item = pair.item;
      var isChunked = !!(chunked && file.size > chunked.SIMPLE_LIMIT);
      if (isChunked) {
        return uploadChunkedFile(file, sub, vol, item);
      }
      if (item) {
        item.state = 'running';
        item.row.setAttribute('data-state', 'running');
        item.pause.hidden = true; // 小文件不可暂停（一次性请求），只留取消
      }
      return uploadSimpleFile(file, sub, vol).then(function () {
        if (item) { item.state = 'done'; item.row.remove(); pruneUploadsBox(); sumUploads(); }
      }, function (err) {
        // **小文件失败也要有失败态**（否则行卡在「排队中」）。
        if (item) { markFailed(item, file, err, function () { attempt(pair); }); }
      });
    };
    // **有限并发**（默认 3）：此前严格串行 —— N 个文件 = N 段带宽空转（每段都要
    // 请求/校验往返，尤其分块上传的 init/complete）。服务端分块会话本就按会话隔离
    // （每会话独立 upload_id），串行只浪费往返延迟。
    // 上限固定 3 而非可配置：再高会抢带宽（「避免并发抢带宽」正是当初串行的理由），
    // 3 是「填满往返空隙」与「不抢带宽」之间的取舍（单文件仍可占满链路）。
    return window.v2uploadPool.runPool(items, attempt, UPLOAD_CONCURRENCY).then(function () {
      if (view()) { view().load(); }
    });
  }

  /** 上传并发上限（固定值，见 uploadFiles 里的权衡说明）。 */
  var UPLOAD_CONCURRENCY = 3;

  function mkdir() {
    // 建目录与上传同一纪律：必须有**具体卷**。不带 `?volume=` 时服务端按 prefer-default
    // 选首个视图卷（`TestMkdir_NoVolumeLandsOnFirstViewVolume` 锁定），在聚合视图里
    // 「建到哪张卷」对用户不可预期 ⇒ 入口已禁用（syncUploadGate），这里再兜底一次。
    var vol = view() && view().volume ? view().volume() : '';
    if (!f.canUploadTo(vol)) {
      toast('warn', '请先选择具体卷再新建目录（「全部卷」是多卷聚合视图，无法确定创建位置）');
      return;
    }
    openDialog({
      title: T('dlg.mkdir.title'), message: T('dlg.mkdir.msg'),
      input: true, value: '', okText: T('dlg.create'),
      onOk: function (name) {
        var rel = relOf(name);
        // **必须带 volume**：否则目录落到默认卷，用户在选中的卷上「建完看不到」。
        run(api.json('POST', '/mkdir?dirname=' + enc(rel) + '&volume=' + enc(vol)),
          '已创建目录 ' + name);
      }
    });
  }

  function doDelete(name, isDir, checksum) {
    var sub = view() ? view().subdir() : '';
    var rel = relOf(name);

    confirmDanger({
      title: isDir ? T('dlg.delDir.title') : T('dlg.delFile.title'),
      message: (isDir ? T('dlg.delDir.msg') : T('dlg.delFile.msg')) + rel,
      danger: true, okText: T('dlg.delete'),
      onOk: function () {
        // 页面按用户配置（设置 → 删除默认方式）**显式**传 soft：
        // 服务端 API 默认语义保持不变（CLI/脚本零回归），配置只决定页面默认行为。
        // 目录删除**不带 volume**：本地卷的 rmdir 刻意按「逻辑目录」语义处理——
        // 同一子目录可因换卷在多卷并存，删「目录 X」应删干净所有卷上的副本（否则留孤儿）
        // （见 pkg/files/write_ops.go RemoveDir 注释与 TestRmdir_LocalVolumeDeletesLogicalDir）。
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
  function deleteMode() { return deleteModeCache === 'hard' ? 'hard' : 'soft'; }

  /** 读取页面配置（失败 = soft 安全默认）。 */
  function loadDeleteMode() {
    if (!api) { return Promise.resolve(); }
    return api.json('GET', '/api/ui/prefs')
      .then(function (body) {
        var d = body && body.prefs && body.prefs['delete'];
        deleteModeCache = (d && d.default_mode === 'hard') ? 'hard' : 'soft';
        skipConfirmCache = !!(d && d.skip_confirm);
      })
      .catch(function () { deleteModeCache = 'soft'; skipConfirmCache = false; });
  }
  window.v2deleteMode = {
    mode: deleteMode,
    reload: loadDeleteMode,
    /** 是否跳过删除二次确认（设置页开关；缺省 false）。 */
    skipConfirm: function () { return skipConfirmCache; }
  };

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
        confirmDanger({
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
              // 目录必须逐个 /rmdir（无批量族）；不需要 checksum 头。**带 volume** 锁定单卷。
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
