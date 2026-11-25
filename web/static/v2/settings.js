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

  // login.js 的凭据端点走 sclientTransport.coreRequest（旧版由 app.js 注入）；v2 用裸 fetch
  // 适配同一协议：公开端点（/api/credentials/*）不需签名，返回 {status, body} 供其解析。
  if (!window.sclientTransport) {
    window.sclientTransport = {
      coreRequest: function (method, path, opts) {
        var o = opts || {};
        var init = { method: method, credentials: 'same-origin', headers: Object.assign({}, o.headers || {}) };
        if (o.bodyBytes) { init.body = o.bodyBytes; }
        return fetch(path, init).then(function (resp) {
          return resp.arrayBuffer().then(function (buf) {
            return { status: resp.status, body: new Uint8Array(buf) };
          });
        });
      }
    };
  }

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

  function toggleRegister(force) {
    var box = el('v2-register');
    if (!box) { return; }
    var show = force === undefined ? box.hidden : force;
    box.hidden = !show;
    if (show) {
      var o = el('v2-reg-owner');
      if (o) { o.focus(); }
    }
  }

  /** 注册：生成 TOTP 密钥 → 展示 AK/Base32/otpauth + 二维码（复用 qrcode.js）。 */
  function register() {
    if (typeof window.registerTOTPByCore !== 'function') { banner('登录模块未加载', 'warn'); return; }
    var owner = ((el('v2-reg-owner') || {}).value || '').trim();
    var box = el('v2-reg-result');
    if (box) { box.textContent = '生成中…'; }
    banner('');
    window.registerTOTPByCore({}, owner)
      .then(function (data) {
        if (!box) { return; }
        box.textContent = '';
        var d = data || {};
        var add = function (label, text, mono) {
          var p = document.createElement('div');
          p.className = 'muted';
          p.style.cssText = 'font-size:12px;margin:8px 0 2px';
          p.textContent = label;
          box.appendChild(p);
          var pre = document.createElement('code');
          pre.style.cssText = 'display:block;padding:8px;background:var(--surface-2);border:1px solid var(--border);' +
            'border-radius:6px;font-size:12px;word-break:break-all';
          pre.textContent = text || '—';
          box.appendChild(pre);
          if (mono) { /* 占位：保持签名一致 */ }
        };
        if (d.admin) {
          var adm = document.createElement('div');
          adm.className = 'banner';
          adm.textContent = '你是首个注册用户，将成为管理员';
          box.appendChild(adm);
        }
        add('AccessKey（请妥善保存）', d.ak);
        add('TOTP 密钥（Base32，录入 Authenticator）', d.base32_secret);
        var qr = document.createElement('div');
        qr.id = 'v2-reg-qr';
        qr.style.cssText = 'margin-top:10px;display:flex;justify-content:center';
        box.appendChild(qr);
        if (d.otpauth_uri && window.sproxyQR && window.sproxyQR.renderToSVG) {
          qr.innerHTML = window.sproxyQR.renderToSVG(d.otpauth_uri);
        }
        add('otpauth 链接', d.otpauth_uri);
        var tip = document.createElement('div');
        tip.className = 'muted';
        tip.style.cssText = 'font-size:12px;margin-top:10px';
        tip.textContent = '注册完成：用上方密钥在身份验证器添加账号，再用动态码登录。密钥只显示这一次。';
        box.appendChild(tip);
        var akInput = el('v2-set-totp-ak');
        if (akInput && d.ak) { akInput.value = d.ak; }
      })
      .catch(function (e) {
        if (box) { box.textContent = ''; }
        banner('注册失败：' + (e.message || e), 'warn');
      });
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
    var rt = el('v2-set-register-toggle'); if (rt) { rt.addEventListener('click', function () { toggleRegister(); }); }
    var rs = el('v2-reg-submit'); if (rs) { rs.addEventListener('click', register); }
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { close(); } });
  }

  window.v2settings = { init: init, open: open, close: close };
  init();
})();
