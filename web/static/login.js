// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Web 注册/登录页（login.js）——TOTP 注册与动态码登录流程。
//
// 依赖（浏览器脚本序，见 index.html；Node 单测经 require 注入）：
//   sclient/crypto.js（sclientCrypto：deriveTOTPWrapKey / aesGcmDecrypt / importAesGcmKey / bytesToHex）
//   sclient/transport.js（sclientTransport：coreRequest / configure）
//   qrcode.js（sproxyQR：renderToMatrix / renderToSVG）
//   app.js（showToast / accessKey / accessKeySecret / accessKeyID / saveAccessKeys）
//
// global（调用点运行期解引用，加载序安全；Node 测试经 globalThis 注入桩）：
//   sclientCrypto（sclient/crypto.js）——TOTP wrap key 派生与 AES-GCM 解密
//   sclientTransport（sclient/transport.js）——公开端点请求 + configure 同步凭据
//   showToast（app.js）
//   accessKey / accessKeySecret / accessKeyID（app.js 顶层 let）
//   applyWebLoginKeys（app.js）——登录成功后统一刷新页面凭据态（输入框/顶层变量/transport）
//   refreshList（app.js）——登录成功后刷新文件列表
//   sproxyQR（qrcode.js）——客户端 QR 渲染（renderToSVG / renderToMatrix）
//
// 端点契约（公开路由，无需签名）：
//   POST /api/credentials/register  body {owner}        → {ak, owner, admin, otpauth_uri, base32_secret}
//   POST /api/credentials/nonce     body 空             → {nonce, expires_at}
//   POST /api/credentials/login     body {ak,nonce,code,login_type} → {ak, session_skey_id, session_expires_at,
//                                                                     wrapped_session_secret:{kind,wrap_key_id,nonce,ciphertext}}
//   信封解密：deriveTOTPWrapKey(code, ak, nonce) 派生 AES-256 密钥，解密 base64 nonce/ciphertext → session SK。
//
// 登录成功回填（简报步骤 1/3）：
//   sessionStorage：sproxy_access_key / sproxy_access_key_secret / sproxy_access_key_id（三键）+ sproxy_last_ak
//   sclientTransport.configure({ accessKey, accessKeySecret, accessKeyID }) —— 同步传输层精确匹配 SK 条目。
//   app.js 侧输入框与顶层变量经 applyWebLoginKeys（app.js 提供）统一刷新。
'use strict';

// ---- 基础工具 ----
// base64 → Uint8Array（浏览器 atob；Node 用 Buffer 兼容）。
function b64ToBytes(b64) {
  if (typeof Buffer !== 'undefined' && typeof Buffer.from === 'function') {
    return new Uint8Array(Buffer.from(b64, 'base64'));
  }
  return Uint8Array.from(atob(b64), function (c) { return c.charCodeAt(0); });
}

// 供 module.exports 引用的 bytesToHex 委托（浏览器直接引用 sclientCrypto）。
function sclientCryptoBytesToHex(u8) {
  if (typeof sclientCrypto !== 'undefined' && sclientCrypto.bytesToHex) return sclientCrypto.bytesToHex(u8);
  throw new Error('sclientCrypto 不可用');
}

// HTML 转义（局部辅助；不重名 appRender.escHtml）。
function htmlEsc(s) {
  return String(s == null ? '' : s)
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
}

// Node 单测导出（浏览器中这些函数本身是脚本级全局，无需该分支）。
if (typeof module === 'object' && module.exports) {
  module.exports = {
    registerTOTPByCore,
    requestTOTPNonceByCore,
    loginTOTPByCore,
    renderRegisterResultHtml,
    renderQRInto,
    b64ToBytes,
    bytesToHex: sclientCryptoBytesToHex,
  };
}

// ---- 公开端点请求（无需签名；transport 可能已配置凭据，公开路由忽略 Authorization） ----
function resolveCrypto() {
  return (typeof sclientCrypto !== 'undefined') ? sclientCrypto : require('./sclient/crypto.js');
}

function resolveTransport() {
  return (typeof sclientTransport !== 'undefined') ? sclientTransport : null;
}

// 请求辅助：POST JSON 到公开凭据端点，解析响应；非 2xx 抛带 status 的错误。
async function credentialRequest(core, method, path, bodyObj) {
  const coreFn = core || (resolveTransport() && resolveTransport().coreRequest);
  if (!coreFn) throw new Error('sclientTransport 不可用（未注入 coreRequest）');
  const bodyBytes = new TextEncoder().encode(JSON.stringify(bodyObj || {}));
  const res = await coreFn(method, path, { bodyBytes, headers: { 'Content-Type': 'application/json' } });
  const text = new TextDecoder().decode(res && res.body ? res.body : new Uint8Array(0));
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch (e) { data = {}; }
  if (res && res.status && (res.status < 200 || res.status >= 300)) {
    const err = new Error((data && data.error) || ('HTTP ' + res.status));
    err.status = res.status;
    throw err;
  }
  return data;
}

// ---- TOTP 注册 ----
async function registerTOTPByCore(ctx, owner) {
  const core = (ctx && ctx.core) || null;
  return credentialRequest(core, 'POST', '/api/credentials/register', { owner: owner || '' });
}

// ---- 申请登录 nonce ----
async function requestTOTPNonceByCore(ctx) {
  const core = (ctx && ctx.core) || null;
  return credentialRequest(core, 'POST', '/api/credentials/nonce', {});
}

// ---- TOTP 动态码登录：nonce → login → 解密 session SK → 回填 ----
async function loginTOTPByCore(ctx, ak, code, loginType) {
  const core = (ctx && ctx.core) || null;
  const cryptoLib = resolveCrypto();
  if (!ak || !code) throw new Error('请输入 AccessKey 与动态码');

  // 1) 申请一次性 nonce
  const nonceResp = await requestTOTPNonceByCore({ core });
  const nonce = nonceResp.nonce;
  if (!nonce) throw new Error('nonce 响应缺少 nonce 字段');

  // 2) 登录（login_type=web/cli；服务端按类型控 session TTL）
  const loginResp = await credentialRequest(core, 'POST', '/api/credentials/login', {
    ak, nonce, code, login_type: loginType || 'web',
  });
  const wrapped = loginResp.wrapped_session_secret;
  if (!wrapped || wrapped.kind !== 'totp_wrap') throw new Error('登录响应缺少 totp_wrap 信封');

  // 3) 解密 session SK：wrapKey = HKDF(sha256(code), "sproxy-accesskey-wrap/v1\0sproxy-totp/v1#nonce", ak)
  const wrapKey = await cryptoLib.deriveTOTPWrapKey(code, ak, nonce);
  const aesKey = await cryptoLib.importAesGcmKey(cryptoLib.bytesToHex(wrapKey));
  const plain = await cryptoLib.aesGcmDecrypt(
    aesKey,
    b64ToBytes(wrapped.nonce),
    b64ToBytes(wrapped.ciphertext)
  );
  const sessionSKHex = cryptoLib.bytesToHex(plain);

  const result = {
    ak: loginResp.ak || ak,
    sessionSKHex,
    sessionSkeyID: loginResp.session_skey_id || '',
    sessionExpiresAt: loginResp.session_expires_at || '',
  };

  // 4) 回填 sessionStorage 三键 + sproxy_last_ak（Node 测试注入 sessionStorage 桩）
  try {
    const st = (typeof sessionStorage !== 'undefined') ? sessionStorage : null;
    if (st) {
      st.setItem('sproxy_access_key', result.ak);
      st.setItem('sproxy_access_key_secret', result.sessionSKHex);
      st.setItem('sproxy_access_key_id', result.sessionSkeyID);
      st.setItem('sproxy_last_ak', result.ak);
    }
  } catch (e) { /* 隐私模式等：跳过持久化，不阻断流程 */ }

  // 5) 同步 transport（accessKeyID = session skey id 精确匹配；refreshList 等后续请求生效）
  try {
    const tr = resolveTransport();
    if (tr && typeof tr.configure === 'function') {
      tr.configure({ accessKey: result.ak, accessKeySecret: result.sessionSKHex, accessKeyID: result.sessionSkeyID, mode: undefined, tunnelDefault: undefined });
    }
  } catch (e) { /* ignore */ }

  // 6) 通知 app.js 刷新页面凭据态（存在时；Node 测试注入桩或忽略）
  if (typeof applyWebLoginKeys === 'function') {
    try { applyWebLoginKeys(result.ak, result.sessionSKHex, result.sessionSkeyID); } catch (e) { /* ignore */ }
  }

  return result;
}

// ---- 注册结果渲染（纯函数，供表单提交后写入结果区） ----
// 输出：base32_secret 文本 + otpauth_uri 链接 + admin 提示 + QR 挂载点（id="qr-register"）。
function renderRegisterResultHtml(data) {
  const d = data || {};
  let html = '';
  html += '<div class="login-result-block">';
  html += '<p style="margin:4px 0;font-size:13px;color:var(--text-secondary);">AccessKey（请妥善保存）</p>';
  html += '<code style="display:block;padding:8px;background:var(--bg-hover);border-radius:4px;font-size:13px;word-break:break-all;">' + htmlEsc(d.ak || '') + '</code>';
  if (d.admin) {
    html += '<p style="margin:8px 0 4px;font-size:13px;color:var(--btn-success-hover);">您是首个注册用户，将成为管理员</p>';
  }
  html += '<p style="margin:8px 0 4px;font-size:13px;color:var(--text-secondary);">TOTP 密钥（Base32，请录入 Authenticator）</p>';
  html += '<code style="display:block;padding:8px;background:var(--bg-hover);border-radius:4px;font-size:13px;word-break:break-all;">' + htmlEsc(d.base32_secret || '') + '</code>';
  if (d.otpauth_uri) {
    html += '<p style="margin:8px 0 4px;font-size:13px;color:var(--text-secondary);">otpauth 链接</p>';
    // 防御性 scheme 校验：仅 otpauth:// 渲染为可点击链接；其它（异常/被篡改数据）输出纯文本防钓鱼/XSS。
    if (String(d.otpauth_uri).slice(0, 11) === 'otpauth://') {
      html += '<a href="' + htmlEsc(d.otpauth_uri) + '" target="_blank" rel="noopener" style="font-size:13px;word-break:break-all;">' + htmlEsc(d.otpauth_uri) + '</a>';
    } else {
      html += '<span style="font-size:13px;word-break:break-all;">' + htmlEsc(d.otpauth_uri) + '</span>';
    }
  }
  html += '<div id="qr-register" style="margin-top:8px;"></div>';
  html += '<p style="margin:8px 0 0;font-size:12px;color:var(--text-muted);">注册完成，请使用上方密钥在身份验证器中添加账号，然后切到「登录」标签用动态码登录。</p>';
  html += '</div>';
  return html;
}

// ---- DOM 集成（浏览器；Node require 时 document 未定义则跳过绑定） ----
if (typeof document !== 'undefined') {
  document.addEventListener('DOMContentLoaded', function () {
    var loginBtn = document.getElementById('login-btn');
    if (loginBtn) loginBtn.addEventListener('click', function () { openLoginModal(); });
    var closeBtn = document.getElementById('login-close-btn');
    if (closeBtn) closeBtn.addEventListener('click', function () { closeLoginModal(); });
    var loginTab = document.getElementById('login-tab');
    if (loginTab) loginTab.addEventListener('click', function () { switchLoginTab('login'); });
    var registerTab = document.getElementById('register-tab');
    if (registerTab) registerTab.addEventListener('click', function () { switchLoginTab('register'); });
    var doLoginBtn = document.getElementById('do-login-btn');
    if (doLoginBtn) doLoginBtn.addEventListener('click', function () { doLogin(); });
    // FF1：登录表单 Enter 提交（键盘可访问性）——onsubmit 返回 false 已阻默认刷新，
    // 这里再显式 preventDefault + 调 doLogin（按钮与表单共用同一入口，避免重复触发）。
    var loginForm = document.getElementById('login-form');
    if (loginForm) loginForm.addEventListener('submit', function (e) {
      e.preventDefault();
      doLogin();
    });
    var doRegisterBtn = document.getElementById('do-register-btn');
    if (doRegisterBtn) doRegisterBtn.addEventListener('click', function () { doRegister(); });
  });
}

function openLoginModal() {
  var modal = document.getElementById('login-modal');
  if (!modal) return;
  modal.style.display = 'flex';
  switchLoginTab('login');
  // 最近 AK 记忆（S3）：sproxy_last_ak 预填 AK 输入框
  try {
    var last = sessionStorage.getItem('sproxy_last_ak');
    var akInput = document.getElementById('login-ak');
    if (last && akInput && !akInput.value) akInput.value = last;
  } catch (e) { /* ignore */ }
}

function closeLoginModal() {
  var modal = document.getElementById('login-modal');
  if (modal) modal.style.display = 'none';
}

function switchLoginTab(tab) {
  var loginPanel = document.getElementById('login-panel');
  var registerPanel = document.getElementById('register-panel');
  var loginTab = document.getElementById('login-tab');
  var registerTab = document.getElementById('register-tab');
  if (loginPanel) loginPanel.style.display = tab === 'login' ? 'block' : 'none';
  if (registerPanel) registerPanel.style.display = tab === 'register' ? 'block' : 'none';
  if (loginTab) {
    loginTab.style.borderBottomColor = tab === 'login' ? 'var(--tab-active)' : 'transparent';
    loginTab.style.color = tab === 'login' ? 'var(--text-primary)' : 'var(--text-secondary)';
  }
  if (registerTab) {
    registerTab.style.borderBottomColor = tab === 'register' ? 'var(--tab-active)' : 'transparent';
    registerTab.style.color = tab === 'register' ? 'var(--text-primary)' : 'var(--text-secondary)';
  }
}

// 渲染二维码（委托 sproxyQR.renderToMatrix → 内联 SVG）。
function renderQRInto(el, otpauthUri) {
  if (!el) return;
  try {
    if (typeof sproxyQR !== 'undefined' && sproxyQR.renderToSVG) {
      el.innerHTML = sproxyQR.renderToSVG(otpauthUri);
      return;
    }
    if (typeof sproxyQR !== 'undefined' && sproxyQR.renderToMatrix) {
      const m = sproxyQR.renderToMatrix(otpauthUri);
      // 退路：像素网格
      let h = '<div style="display:grid;grid-template-columns:repeat(' + m.length + ',4px);gap:0;">';
      for (let r = 0; r < m.length; r++) {
        for (let c = 0; c < m.length; c++) h += '<div style="width:4px;height:4px;background:' + (m[r][c] ? '#000' : '#fff') + ';"></div>';
      }
      h += '</div>';
      el.innerHTML = h;
    }
  } catch (e) { /* QR 渲染失败不阻断注册结果展示 */ }
}

async function doRegister() {
  var ownerInput = document.getElementById('register-owner');
  var resultEl = document.getElementById('register-result');
  var btn = document.getElementById('do-register-btn');
  if (!ownerInput || !resultEl) return;
  var owner = ownerInput.value.trim();
  if (!owner) { showToast('请输入注册用户名（owner）', 'warning'); return; }
  if (btn) { btn.disabled = true; btn.textContent = '注册中...'; }
  try {
    var data = await registerTOTPByCore({}, owner);
    resultEl.innerHTML = renderRegisterResultHtml(data);
    renderQRInto(document.getElementById('qr-register'), data.otpauth_uri || '');
    switchLoginTab('register');
    if (data.admin) {
      showToast('注册成功：您是首个注册用户，将成为管理员', 'success');
    } else {
      showToast('注册成功，请用密钥添加 TOTP 账号', 'success');
    }
  } catch (e) {
    resultEl.innerHTML = '<div class="empty-msg" style="color:var(--text-danger);">注册失败: ' + htmlEsc(e.message) + '</div>';
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '注册'; }
  }
}

async function doLogin() {
  var akInput = document.getElementById('login-ak');
  var codeInput = document.getElementById('login-code');
  var resultEl = document.getElementById('login-result');
  var btn = document.getElementById('do-login-btn');
  if (!akInput || !codeInput || !resultEl) return;
  var ak = akInput.value.trim();
  var code = codeInput.value.trim();
  if (!ak || !code) { showToast('请输入 AccessKey 与动态码', 'warning'); return; }
  if (btn) { btn.disabled = true; btn.textContent = '登录中...'; }
  try {
    var res = await loginTOTPByCore({}, ak, code, 'web');
    resultEl.innerHTML = '<div style="padding:8px 0;font-size:13px;color:var(--btn-success-hover);">登录成功，会话已建立（expires ' + htmlEsc(res.sessionExpiresAt || '-') + '）</div>';
    closeLoginModal();
    showToast('登录成功，凭据已生效', 'success');
    if (typeof refreshList === 'function') refreshList();
  } catch (e) {
    resultEl.innerHTML = '<div class="empty-msg" style="color:var(--text-danger);">登录失败: ' + htmlEsc(e.message) + '</div>';
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '登录'; }
  }
}
