// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 连接设置：AccessKey/Secret 保存与校验、TOTP 登录（复用 login.js 流程）、
// Web 隧道开关（PUT /api/config web_tunnel）。凭据仍写旧版三键，v2 不新增存储。
'use strict';

(function () {
  if (typeof document === 'undefined') { return; }
  var api = window.v2api, creds = window.sharedCreds;
  if (!api || !creds) { return; }

  function el(id) { return document.getElementById(id); }

  function banner(text, kind) {
    var box = el('v2-set-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function toast(kind, msg) { if (window.v2toast) { window.v2toast(kind, msg); } }

  function fillCreds() {
    var c = creds.loadCreds(window.sessionStorage);
    var ak = el('v2-set-ak'), sk = el('v2-set-sk'), id = el('v2-set-id');
    if (ak) { ak.value = c ? c.ak : ''; }
    if (sk) { sk.value = c ? c.sk : ''; }
    if (id) { id.value = c ? c.entryID : ''; }
    var last = el('v2-set-totp-ak');
    if (last && !last.value) {
      var v = '';
      try { v = sessionStorage.getItem('sproxy_last_ak') || ''; } catch (e) { v = ''; }
      last.value = v;
    }
  }

  function saveCreds() {
    var ak = (el('v2-set-ak') || {}).value || '';
    var sk = (el('v2-set-sk') || {}).value || '';
    var id = (el('v2-set-id') || {}).value || '';
    if (!ak.trim() || !sk.trim()) { banner('AccessKey 与 Secret 都必须填写。', 'warn'); return; }
    var prev = creds.loadCreds(window.sessionStorage);
    try {
      sessionStorage.setItem(creds.KEYS.ak, ak.trim());
      sessionStorage.setItem(creds.KEYS.sk, sk.trim());
      sessionStorage.setItem(creds.KEYS.id, id.trim());
      sessionStorage.setItem('sproxy_last_ak', ak.trim());
    } catch (e) { banner('浏览器拒绝写入会话存储：' + e.message, 'warn'); return; }
    banner('正在校验凭据…');
    api.json('GET', '/api/files?limit=1')
      .then(function () {
        banner('');
        toast('ok', '凭据已保存并通过校验');
        if (window.v2filesView) { window.v2filesView.load(); }
        loadTunnel();
      })
      .catch(function (e) {
        // 校验失败回滚，避免留下错误凭据导致全站 401。
        try {
          if (prev) {
            sessionStorage.setItem(creds.KEYS.ak, prev.ak);
            sessionStorage.setItem(creds.KEYS.sk, prev.sk);
            sessionStorage.setItem(creds.KEYS.id, prev.entryID);
          } else {
            sessionStorage.removeItem(creds.KEYS.ak);
            sessionStorage.removeItem(creds.KEYS.sk);
            sessionStorage.removeItem(creds.KEYS.id);
          }
        } catch (ignore) { /* 忽略 */ }
        banner('凭据校验失败，已回滚：' + (e.message || e), 'warn');
      });
  }

  function signOut() {
    try {
      sessionStorage.removeItem(creds.KEYS.ak);
      sessionStorage.removeItem(creds.KEYS.sk);
      sessionStorage.removeItem(creds.KEYS.id);
    } catch (e) { /* 忽略 */ }
    fillCreds();
    banner('已退出（凭据已清除）。');
    toast('ok', '已退出登录');
    if (window.v2filesView) { window.v2filesView.load(); }
  }

  function totpLogin() {
    var ak = ((el('v2-set-totp-ak') || {}).value || '').trim();
    var code = ((el('v2-set-totp-code') || {}).value || '').trim();
    if (!ak || !code) { banner('请输入 AccessKey/用户名与动态码。', 'warn'); return; }
    if (typeof window.loginTOTPByCore !== 'function') { banner('登录模块未加载，请使用旧版登录。', 'warn'); return; }
    banner('登录中…');
    window.loginTOTPByCore({}, ak, code, 'web')
      .then(function (res) {
        banner('');
        toast('ok', '登录成功（会话到期 ' + (res.sessionExpiresAt || '-') + '）');
        fillCreds();
        loadTunnel();
        if (window.v2filesView) { window.v2filesView.load(); }
      })
      .catch(function (e) { banner('登录失败：' + (e.message || e), 'warn'); });
  }

  function loadTunnel() {
    var cb = el('v2-set-tunnel');
    if (!cb) { return; }
    api.json('GET', '/api/config')
      .then(function (cfg) { cb.checked = !!(cfg && cfg.web_tunnel); })
      .catch(function () { /* 未授权时保持默认关闭 */ });
  }

  function saveTunnel() {
    var cb = el('v2-set-tunnel');
    if (!cb) { return; }
    api.json('PUT', '/api/config', {
      body: new TextEncoder().encode(JSON.stringify({ web_tunnel: !!cb.checked })),
      contentType: 'application/json'
    }).then(function () { toast('ok', '隧道开关已保存'); })
      .catch(function (e) { banner('保存隧道开关失败：' + (e.message || e), 'warn'); });
  }

  function open() {
    var box = el('v2-settings');
    if (!box) { return; }
    box.hidden = false;
    banner('');
    fillCreds();
    loadTunnel();
    var ak = el('v2-set-ak');
    if (ak) { ak.focus(); }
  }

  function close() {
    var box = el('v2-settings');
    if (box) { box.hidden = true; }
  }

  var inited = false;
  function init() {
    if (inited) { return; }
    inited = true;
    var openBtn = el('v2-settings-open');
    if (openBtn) { openBtn.addEventListener('click', open); }
    var closeBtn = el('v2-settings-close'); if (closeBtn) { closeBtn.addEventListener('click', close); }
    var back = el('v2-settings-back'); if (back) { back.addEventListener('click', close); }
    var save = el('v2-set-save'); if (save) { save.addEventListener('click', saveCreds); }
    var out = el('v2-set-signout'); if (out) { out.addEventListener('click', signOut); }
    var login = el('v2-set-login'); if (login) { login.addEventListener('click', totpLogin); }
    var tSave = el('v2-set-tunnel-save'); if (tSave) { tSave.addEventListener('click', saveTunnel); }
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { close(); } });
  }

  window.v2settings = { init: init, open: open, close: close };
  init();
})();
