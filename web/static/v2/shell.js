// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 壳层（P0）：路由解析、主题决策、导航渲染。
// 纯函数与 DOM 分离：Node 下可 require 做单测，浏览器下自执行初始化。
'use strict';

(function () {
  var ROUTES = ['files', 'transfer', 'trash', 'console'];
  var DEFAULT_ROUTE = 'files';
  var THEMES = ['light', 'dark'];
  var THEME_KEY = 'sproxy_v2_theme';

  /** 解析 hash/路径为合法路由；未知或空一律回落 files。 */
  function parseRoute(hash) {
    if (!hash) { return DEFAULT_ROUTE; }
    var raw = String(hash).replace(/^#/, '').replace(/^\//, '').replace(/\/+$/, '');
    if (!raw) { return DEFAULT_ROUTE; }
    var name = raw.split('/')[0];
    return ROUTES.indexOf(name) === -1 ? DEFAULT_ROUTE : name;
  }

  /** 路由 → hash 链接。 */
  function routeHref(name) {
    return '#/' + (ROUTES.indexOf(name) === -1 ? DEFAULT_ROUTE : name);
  }

  /** 侧栏导航项（单一事实源，测试与 DOM 共用）。 */
  function navItems() {
    return [
      { group: '工作区', id: 'files', label: '文件', count: 128 },
      { group: '工作区', id: 'transfer', label: '传输', count: 3 },
      { group: '工作区', id: 'trash', label: '回收站', count: 4 },
      { group: '系统', id: 'console', label: '控制台' }
    ];
  }

  /** 主题决策：显式 saved 优先，否则跟随系统。 */
  function resolveTheme(saved, prefersDark) {
    if (THEMES.indexOf(saved) !== -1) { return saved; }
    return prefersDark ? 'dark' : 'light';
  }

  var TITLES = {
    files: ['文件', '/ 全部卷'],
    transfer: ['传输', '/ 3 个进行中'],
    trash: ['回收站', '/ 保留 30 天'],
    console: ['控制台', '/ 概览']
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      ROUTES: ROUTES,
      DEFAULT_ROUTE: DEFAULT_ROUTE,
      parseRoute: parseRoute,
      routeHref: routeHref,
      navItems: navItems,
      resolveTheme: resolveTheme,
      TITLES: TITLES
    };
  }

  if (typeof document === 'undefined') { return; }

  function el(id) { return document.getElementById(id); }

  function applyTheme(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    try { localStorage.setItem(THEME_KEY, theme); } catch (e) { /* 隐私模式忽略 */ }
  }

  function renderRoute() {
    var route = parseRoute(window.location.hash);
    Array.prototype.forEach.call(document.querySelectorAll('[data-route]'), function (node) {
      var active = node.getAttribute('data-route') === route;
      node.setAttribute('aria-current', active ? 'page' : 'false');
      if (!active) { node.removeAttribute('aria-current'); }
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-panel]'), function (panel) {
      panel.hidden = panel.getAttribute('data-panel') !== route;
    });
    var t = TITLES[route] || TITLES[DEFAULT_ROUTE];
    var title = el('v2-title');
    var sub = el('v2-subtitle');
    if (title) { title.textContent = t[0]; }
    if (sub) { sub.textContent = t[1]; }
    document.title = t[0] + ' · sproxy v2';
    if (window.location.hash !== routeHref(route)) {
      try { history.replaceState(null, '', routeHref(route)); } catch (e) { /* file:// */ }
    }
  }

  function init() {
    var saved = null;
    try { saved = localStorage.getItem(THEME_KEY); } catch (e) { saved = null; }
    var prefersDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
    applyTheme(resolveTheme(saved, prefersDark));

    var toggle = el('v2-theme-toggle');
    if (toggle) {
      toggle.addEventListener('click', function () {
        var next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
        applyTheme(next);
      });
    }

    Array.prototype.forEach.call(document.querySelectorAll('[data-route]'), function (node) {
      node.addEventListener('click', function () {
        window.location.hash = routeHref(node.getAttribute('data-route'));
        renderRoute(); // 立即渲染，避免 e2e/键盘操作等待 hashchange
      });
    });

    window.addEventListener('hashchange', renderRoute);
    renderRoute();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
