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
  var state = { name: '', url: '' };

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

  function resetStage() {
    var img = el('v2-pv-img'), video = el('v2-pv-video'), text = el('v2-pv-text'), msg = el('v2-pv-msg');
    img.hidden = true; img.removeAttribute('src');
    video.hidden = true; video.pause(); video.removeAttribute('src'); video.load();
    text.hidden = true; text.textContent = '';
    msg.hidden = true; msg.textContent = '';
  }

  function open(name) {
    var file = view() ? view().file(name) : null;
    if (!file) { return; }
    state.name = name;
    var sub = view().subdir();
    var rel = f.joinSubdir(sub, name);
    syncCredGlobals();
    resetStage();
    el('v2-pv-title').textContent = name;
    el('v2-pv-meta').textContent = (file.volume || '') + (file.isDir ? ' · 目录' : ' · ' + f.formatSize(file.size));
    var overlay = el('v2-preview');
    overlay.hidden = false;
    document.body.classList.add('pv-open');

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
          text.textContent = body.length > TEXT_LIMIT ? body.slice(0, TEXT_LIMIT) + '\n…（已截断）' : body;
        })
        .catch(function (e) { text.textContent = '读取失败：' + (e.message || e); });
      return;
    }
    msg.hidden = false;
    msg.textContent = (window.isBrowserPlayable && !window.isBrowserPlayable(name) && /\.(mp4|mkv|avi|mov|webm|wmv|flv|rmvb)$/i.test(name))
      ? '该视频格式浏览器不支持播放，请下载后用本地播放器打开。'
      : '该类型暂不支持在线预览，请下载查看。';
  }

  function close() {
    var overlay = el('v2-preview');
    if (!overlay || overlay.hidden) { return; }
    overlay.hidden = true;
    document.body.classList.remove('pv-open');
    resetStage();
  }

  function download(name) {
    var file = view() ? view().file(name) : null;
    if (!file) { return; }
    var rel = f.joinSubdir(view().subdir(), name);
    syncCredGlobals();
    signedUrl(rel, file.volume).then(function (url) {
      var a = document.createElement('a');
      a.href = url; a.download = name;
      document.body.appendChild(a); a.click(); a.remove();
    }).catch(function (e) { if (window.v2toast) { window.v2toast('bad', '下载失败：' + e.message); } });
  }

  function init() {
    el('v2-pv-close').addEventListener('click', close);
    el('v2-pv-backdrop').addEventListener('click', close);
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { close(); } });
  }

  window.v2preview = { open: open, close: close, download: download };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
