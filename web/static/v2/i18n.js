// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2 语言切换：壳层/导航/控制台分区等「静态 chrome」的中英词条。
// 动态数据（文件名、任务状态、服务端错误）保持服务端原文，不在此翻译。
'use strict';

(function () {
  var KEY = 'sproxy_v2_lang';
  var DICT = {
    zh: {
      files: '文件', transfer: '传输', trash: '回收站', console: '控制台',
      density: '密度', cmdk: '⌘K', settings: '设置', legacy: '旧版',
      'group.workspace': '工作区', 'group.system': '系统',
      search: '搜索文件名（/ 聚焦）', searchBtn: '搜索', clear: '清除',
      upload: '上传', mkdir: '新建目录', deleteSel: '删除所选', renameSel: '重命名所选',
      archiveSel: '打包下载', refresh: '刷新', volRefresh: '清缓存',
      'cs.overview': '概览', 'cs.tasks': '云端任务', 'cs.issues': '待处理事项',
      'cs.audit': '审计', 'cs.config': '配置', 'cs.notify': '通知', 'cs.verify': '巡检',
      'cs.access': '接入', 'cs.volumes': '卷', 'cs.mesh': 'Mesh', 'cs.hub': 'Hub',
      'title.files': '文件', 'title.transfer': '传输', 'title.trash': '回收站', 'title.console': '控制台'
    },
    en: {
      files: 'Files', transfer: 'Transfer', trash: 'Trash', console: 'Console',
      density: 'Density', cmdk: '⌘K', settings: 'Settings', legacy: 'Legacy',
      'group.workspace': 'Workspace', 'group.system': 'System',
      search: 'Search files (/ to focus)', searchBtn: 'Search', clear: 'Clear',
      upload: 'Upload', mkdir: 'New folder', deleteSel: 'Delete selected', renameSel: 'Rename selected',
      archiveSel: 'Download archive', refresh: 'Refresh', volRefresh: 'Clear cache',
      'cs.overview': 'Overview', 'cs.tasks': 'Cloud tasks', 'cs.issues': 'Issues',
      'cs.audit': 'Audit', 'cs.config': 'Config', 'cs.notify': 'Notify', 'cs.verify': 'Verify',
      'cs.access': 'Access', 'cs.volumes': 'Volumes', 'cs.mesh': 'Mesh', 'cs.hub': 'Hub',
      'title.files': 'Files', 'title.transfer': 'Transfer', 'title.trash': 'Trash', 'title.console': 'Console'
    }
  };

  function lang() {
    var v = null;
    try { v = localStorage.getItem(KEY); } catch (e) { v = null; }
    return DICT[v] ? v : 'zh';
  }

  function t(key) {
    var d = DICT[lang()];
    return (d && d[key] != null) ? d[key] : key;
  }

  function routeOf() {
    var raw = String(window.location.hash || '').replace(/^#\/?/, '');
    var first = raw.split('/')[0];
    return DICT[lang()]['title.' + first] ? first : 'files';
  }

  function apply() {
    var d = DICT[lang()];
    Array.prototype.forEach.call(document.querySelectorAll('[data-i18n]'), function (n) {
      var k = n.getAttribute('data-i18n');
      if (d[k] != null) { n.textContent = d[k]; }
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-i18n-ph]'), function (n) {
      var k = n.getAttribute('data-i18n-ph');
      if (d[k] != null) { n.setAttribute('placeholder', d[k]); }
    });
    var title = document.getElementById('v2-title');
    if (title) { title.textContent = t('title.' + routeOf()); }
    var sel = document.getElementById('v2-set-lang');
    if (sel) { sel.value = lang(); }
    document.documentElement.setAttribute('lang', lang() === 'en' ? 'en' : 'zh-CN');
  }

  function setLang(v) {
    try { localStorage.setItem(KEY, DICT[v] ? v : 'zh'); } catch (e) { /* 忽略 */ }
    apply();
    window.dispatchEvent(new CustomEvent('v2:lang', { detail: { lang: lang() } }));
  }

  if (typeof window !== 'undefined') {
    window.v2i18n = { t: t, lang: lang, setLang: setLang, apply: apply };
    window.addEventListener('hashchange', function () {
      var title = document.getElementById('v2-title');
      if (title) { title.textContent = t('title.' + routeOf()); }
    });
    document.addEventListener('DOMContentLoaded', function () {
      apply();
      var sel = document.getElementById('v2-set-lang');
      if (sel) { sel.addEventListener('change', function () { setLang(sel.value); }); }
    });
    if (document.readyState !== 'loading') {
      apply();
      var sel2 = document.getElementById('v2-set-lang');
      if (sel2) { sel2.addEventListener('change', function () { setLang(sel2.value); }); }
    }
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { t: t, lang: lang, DICT: DICT };
  }
})();
