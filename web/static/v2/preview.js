// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 预览（P1e）：图片/视频（URL 票）/文本（签名请求），复用旧版 video-player 的
// videoPlayerUrl / withUrlSig / isBrowserPlayable —— 不复制签名与可播判定逻辑。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var api = window.v2api, auth = window.v2auth, f = window.v2files;
  if (!api || !auth || !f) { return; }

  var TEXT_LIMIT = 512 * 1024;
  var state = { name: '', url: '', mode: 'file' };

  function el(id) { return document.getElementById(id); }
  function enc(s) { return encodeURIComponent(s); }
  function view() { return window.v2filesView; }
  function ext(name) { var i = String(name).lastIndexOf('.'); return i < 0 ? '' : String(name).slice(i).toLowerCase(); }

  function syncCredGlobals() {
    var c = auth.loadCreds(window.sessionStorage);
    window.accessKey = c ? c.ak : '';
    window.accessKeySecret = c ? c.sk : '';
    window.accessKeyID = c ? (c.entryID || '') : '';
  }

  function signedUrl(rel, volume) {
    var raw = window.videoPlayerUrl({ filename: rel, volume: volume || '' });
    var q = raw.indexOf('?') >= 0 ? raw.slice(raw.indexOf('?') + 1) : '';
    return window.withUrlSig(q).then(function (signed) { return '/download?' + signed; });
  }

  /** 悬浮关闭按钮：与头部关闭同源，保证视频控件/全屏态下也能关闭。 */
  function showFloatClose(on) {
    var f = el('v2-pv-float-close');
    if (f) { f.hidden = !on; }
  }

  function resetStage() {
    var img = el('v2-pv-img'), video = el('v2-pv-video'), text = el('v2-pv-text'), msg = el('v2-pv-msg');
    img.hidden = true; img.removeAttribute('src');
    video.hidden = true; video.pause(); video.removeAttribute('src'); video.load();
    text.hidden = true; text.textContent = '';
    msg.hidden = true; msg.textContent = '';
  }

  /** 当前列表顺序里的相邻文件（跳过目录）；到边界不动。 */
  function neighbor(delta) {
    var v = view();
    if (!v || !state.name || state.mode !== 'file') { return ''; }
    var list = v.state ? v.state.files : [];
    return f.neighborFile(list, state.name, delta);
  }

  /** 切到相邻文件（保持预览打开）。 */
  function step(delta) {
    var name = neighbor(delta);
    if (!name) { return; }
    open(name);
  }

  /** 刷新「上一张/下一张」按钮的可用状态（标题旁展示位置 i/n）。 */
  function syncSteps() {
    var prev = el('v2-pv-prev');
    var next = el('v2-pv-next');
    var pos = el('v2-pv-pos');
    var hasPrev = !!neighbor(-1);
    var hasNext = !!neighbor(1);
    if (prev) { prev.disabled = !hasPrev; }
    if (next) { next.disabled = !hasNext; }
    if (pos) {
      var list = view() && view().state ? view().state.files : [];
      var files = list.filter(function (x) { return x && !x.isDir; });
      var idx = -1;
      for (var i = 0; i < files.length; i += 1) { if (files[i].name === state.name) { idx = i; break; } }
      pos.textContent = idx >= 0 ? (idx + 1) + ' / ' + files.length : '';
    }
  }

  function open(name) {
    var file = view() ? view().file(name) : null;
    if (!file) { return; }
    state.name = name;
    var sub = view().subdir();
    var rel = f.resolveRel(sub, name, !!(view().search && view().search()));
    syncCredGlobals();
    resetStage();
    el('v2-pv-title').textContent = name;
    el('v2-pv-meta').textContent = (file.volume || '') + (file.isDir ? ' · 目录' : ' · ' + f.formatSize(file.size));
    var overlay = el('v2-preview');
    overlay.hidden = false;
    document.body.classList.add('pv-open');
    showFloatClose(true);
    state.mode = 'file';
    syncSteps();


    var img = el('v2-pv-img'), video = el('v2-pv-video'), text = el('v2-pv-text'), msg = el('v2-pv-msg');
    var type = ext(name);
    var imageType = ['.png', '.jpg', '.jpeg', '.webp', '.gif', '.bmp', '.avif'].indexOf(type) >= 0;
    var textType = ['.txt', '.md', '.json', '.log', '.yaml', '.yml', '.srt', '.csv', '.xml'].indexOf(type) >= 0;

    if (imageType || (window.isBrowserPlayable && window.isBrowserPlayable(name))) {
      signedUrl(rel, file.volume).then(function (url) {
        state.url = url;
        if (imageType) { img.src = url; img.hidden = false; return; }
        video.src = url; video.hidden = false; video.play().catch(function () { /* 自动播放被拒 */ });
      }).catch(function (e) { msg.hidden = false; msg.textContent = '无法生成预览链接：' + (e.message || e); });
      return;
    }
    if (textType) {
      text.textContent = '加载中…';
      text.hidden = false;
      api.request('GET', '/download?' + 'filename=' + enc(rel) + (file.volume ? '&volume=' + enc(file.volume) : ''))
        .then(api.ensureOk)
        .then(function (resp) { return resp.text(); })
        .then(function (body) {
          text.textContent = body.length > TEXT_LIMIT ? body.slice(0, TEXT_LIMIT) + String.fromCharCode(10) + '…（已截断）' : body;
        })
        .catch(function (e) { text.textContent = '读取失败：' + (e.message || e); });
      return;
    }
    msg.hidden = false;
    msg.textContent = (window.isBrowserPlayable && !window.isBrowserPlayable(name) && /\.(mp4|mkv|avi|mov|webm|wmv|flv|rmvb)$/i.test(name))
      ? '该视频格式浏览器不支持播放，请下载后用本地播放器打开。'
      : '该类型暂不支持在线预览，请下载查看。';
  }

  /**
   * 预览云任务产物（cloud 桶 `<taskID>/<filename>`，需 `kind=cloud_task` 由服务端按 owner 拼接）。
   * 与 open() 共用浮层与判定逻辑；差异只在 URL/读取路径都带 kind=cloud_task。
   */
  function openCloudTask(taskID, filename) {
    if (!taskID || !filename) { return; }
    var rel = String(taskID) + '/' + String(filename);
    var name = String(filename);
    state.cloudRel = rel;
    state.name = name;
    syncCredGlobals();
    resetStage();
    el('v2-pv-title').textContent = name;
    el('v2-pv-meta').textContent = '云端任务 · ' + rel;
    var overlay = el('v2-preview');
    overlay.hidden = false;
    document.body.classList.add('pv-open');
    showFloatClose(true);
    // 云任务产物不在文件列表里 ⇒ 没有可切换的相邻文件。
    state.mode = 'cloud_task';
    syncSteps();

    var img = el('v2-pv-img'), video = el('v2-pv-video'), text = el('v2-pv-text'), msg = el('v2-pv-msg');
    var type = ext(name);
    var imageType = ['.png', '.jpg', '.jpeg', '.webp', '.gif', '.bmp', '.avif'].indexOf(type) >= 0;
    var textType = ['.txt', '.md', '.json', '.log', '.yaml', '.yml', '.srt', '.csv', '.xml'].indexOf(type) >= 0;
    var baseQuery = 'filename=' + enc(rel) + '&kind=cloud_task';

    if (imageType || (window.isBrowserPlayable && window.isBrowserPlayable(name))) {
      window.withUrlSig(baseQuery).then(function (signed) {
        var url = '/download?' + signed;
        state.url = url;
        if (imageType) { img.src = url; img.hidden = false; return; }
        video.src = url; video.hidden = false; video.play().catch(function () { /* 自动播放被拒 */ });
      }).catch(function (e) { msg.hidden = false; msg.textContent = '无法生成预览链接：' + (e.message || e); });
      return;
    }
    if (textType) {
      text.textContent = '加载中…';
      text.hidden = false;
      api.request('GET', '/download?' + baseQuery)
        .then(api.ensureOk)
        .then(function (resp) { return resp.text(); })
        .then(function (body) {
          text.textContent = body.length > TEXT_LIMIT ? body.slice(0, TEXT_LIMIT) + String.fromCharCode(10) + '…（已截断）' : body;
        })
        .catch(function (e) { text.textContent = '读取失败：' + (e.message || e); });
      return;
    }
    msg.hidden = false;
    msg.textContent = '该类型暂不支持在线预览，请下载查看。';
  }

  function close() {
    var overlay = el('v2-preview');
    if (!overlay || overlay.hidden) { return; }
    overlay.hidden = true;
    document.body.classList.remove('pv-open');
    showFloatClose(false);
    resetStage();
  }

  function download(name) {
    var file = view() ? view().file(name) : null;
    if (!file) { return; }
    var rel = f.resolveRel(view().subdir(), name, !!(view().search && view().search()));
    syncCredGlobals();
    signedUrl(rel, file.volume).then(function (url) {
      var a = document.createElement('a');
      a.href = url; a.download = name;
      document.body.appendChild(a); a.click(); a.remove();
      setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
    }).catch(function (e) { if (window.v2toast) { window.v2toast('bad', '下载失败：' + e.message); } });
  }

  function init() {
    el('v2-pv-close').addEventListener('click', close);
    var fc = el('v2-pv-float-close');
    if (fc) { fc.addEventListener('click', close); }
    el('v2-pv-backdrop').addEventListener('click', close);
    // 点击舞台空白处关闭（视频/图片区域自身不关闭，避免误触暂停/选中）。
    var stage = el('v2-pv-stage');
    if (stage) {
      stage.addEventListener('click', function (e) { if (e.target === stage) { close(); } });
    }
    // 头部「下载」：按当前预览对象下载（文件走签名 URL，云任务走 kind=cloud_task）。
    var dl = el('v2-pv-download');
    if (dl) {
      dl.addEventListener('click', function () {
        if (state.mode === 'cloud') { downloadCloud(state.cloudRel || state.name); return; }
        download(state.name);
      });
    }
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { close(); return; }
      // ←/→ 切换同目录的相邻**文件**（跳过目录）——看图/看视频时不必退出重进。
      if (e.target && /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) { return; }
      if (e.key === 'ArrowLeft') { e.preventDefault(); step(-1); }
      else if (e.key === 'ArrowRight') { e.preventDefault(); step(1); }
    });
    var prev = el('v2-pv-prev'); if (prev) { prev.addEventListener('click', function () { step(-1); }); }
    var next = el('v2-pv-next'); if (next) { next.addEventListener('click', function () { step(1); }); }
  }

  /** 云任务产物下载（cloud 桶，kind=cloud_task）。 */
  function downloadCloud(rel) {
    window.withUrlSig('filename=' + enc(rel) + '&kind=cloud_task').then(function (signed) {
      var a = document.createElement('a');
      a.href = '/download?' + signed;
      a.download = rel.split('/').pop();
      document.body.appendChild(a); a.click(); a.remove();
    }).catch(function (e) { if (window.v2toast) { window.v2toast('bad', '下载失败：' + (e.message || e)); } });
  }

  window.v2preview = {
    openCloudTask: openCloudTask, open: open, close: close, download: download };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
