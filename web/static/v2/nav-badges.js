// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 侧栏徽标与账户卡：把**真实**计数与登录身份写到导航上。
//
// 背景：此前 HTML 里硬编码 `128 / 3 / 4` 与写死的占位身份，永不更新，
// 用户看到的是假数据（比不显示更糟）。这里只暴露纯函数 + 一个 set 接口，
// 由各页面在拿到数据后调用（files-view / transfer / trash）。
'use strict';

(function (root, factory) {
  var api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  } else {
    root.v2nav = api;
  }
})(typeof self !== 'undefined' ? self : this, function () {
  /**
   * 徽标文本：null/undefined（未知）⇒ 空串（隐藏）；>99 ⇒ '99+'。
   * 0 也要显示（"回收站 0 项"是有意义的信息，而不是未知）。
   * @param {number|null|undefined} n
   * @returns {string}
   */
  function badgeText(n) {
    if (n === null || n === undefined) { return ''; }
    var v = Number(n);
    if (!isFinite(v) || v < 0) { return ''; }
    return v > 99 ? '99+' : String(Math.floor(v));
  }

  /**
   * 账户卡展示模型。
   * @param {{ak:string, entryID:string}|null} creds 已登录凭据；null = 匿名
   * @returns {{initial:string, name:string, sub:string, anon:boolean}}
   */
  function accountModel(creds) {
    if (!creds || !creds.ak) {
      return { initial: '?', name: '匿名', sub: '未登录（loopback 调试）', anon: true };
    }
    var ak = String(creds.ak);
    // 只显示前 6 位 + 省略：AccessKey 属凭据，避免整串留在屏幕上被截图带走。
    var shown = ak.length > 8 ? ak.slice(0, 6) + '…' : ak;
    return { initial: ak.slice(0, 2).toUpperCase(), name: shown, sub: creds.entryID ? '已登录 · Key ' + creds.entryID : '已登录', anon: false };
  }

  // ---------- DOM 接线（浏览器侧） ----------

  /** 写徽标：空串 ⇒ 隐藏（`hidden` 属性 + 不占位）。 */
  function setBadge(route, n) {
    if (typeof document === 'undefined') { return; }
    var txt = badgeText(n);
    Array.prototype.forEach.call(document.querySelectorAll('[data-badge="' + route + '"]'), function (node) {
      node.textContent = txt;
      node.hidden = txt === '';
    });
  }

  /** 写账户卡（AK 摘要 + 登录态）。 */
  function setAccount(credsOrModel) {
    if (typeof document === 'undefined') { return; }
    var m = credsOrModel && credsOrModel.name ? credsOrModel : accountModel(credsOrModel);
    var ini = document.getElementById('v2-account-initial');
    var name = document.getElementById('v2-account-name');
    var sub = document.getElementById('v2-account-sub');
    if (ini) { ini.textContent = m.initial; }
    if (name) { name.textContent = m.name; }
    if (sub) { sub.textContent = m.sub; }
    var who = document.getElementById('v2-account');
    if (who) { who.setAttribute('title', m.anon ? '未登录：仅 loopback 匿名访问' : (m.sub || '')); }
  }

  /** 从 sessionStorage 读凭据并刷新账户卡。 */
  function refreshAccount() {
    if (typeof document === 'undefined') { return; }
    var c = null;
    try { c = (typeof root !== 'undefined' && root.sharedCreds) ? root.sharedCreds.loadCreds(root.sessionStorage) : null; } catch (e) { c = null; }
    setAccount(c);
  }

  function init() {
    if (typeof document === 'undefined') { return; }
    refreshAccount();
    // 各视图拿到数据后广播；未知的 route 忽略。
    document.addEventListener('v2:badge', function (ev) {
      var d = ev && ev.detail;
      if (d && d.route) { setBadge(d.route, d.count); }
    });
    // 登录/退出后刷新身份。
    document.addEventListener('v2:identity', refreshAccount);
    // 初始一律清空（HTML 里不再写死数字）。
    setBadge('files', null);
    setBadge('transfer', null);
    setBadge('trash', null);
  }

  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', init);
    } else {
      init();
    }
  }

  return {
    badgeText: badgeText, accountModel: accountModel,
    setBadge: setBadge, setAccount: setAccount, refreshAccount: refreshAccount
  };
});
