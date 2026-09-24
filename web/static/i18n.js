/**
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * i18n.js —— WebUI 多语言框架（roadmap 11.10-H1）
 *
 * 提供：词条表（zh/en）+ t()/fmt() 取词与插值 + lang() 语言解析
 * （localStorage > navigator.language > 默认 zh）+ setLang 持久化与事件
 * 派发 + applyStaticI18n 批量替换 data-i18n 静态文案。
 *
 * 纯函数 + 全局副作用最小化：node --test 可测（stub 全局）。
 */
'use strict';

/* global localStorage, window, navigator, document */

(function (global) {
  'use strict';

  const STORAGE_KEY = 'sproxy_lang';

  const dicts = {
    zh: {
      upload_files: '上传文件',
      confirm_delete: '确认删除 "{name}"?',
      search_placeholder: '搜索文件…',
      // -- 导航/页签 --
      nav_files: '文件',
      nav_transfer: '传输',
      nav_volumes: '卷',
      nav_hub: 'Hub',
      nav_notify: '通知',
      nav_audit: '审计',
      nav_settings: '设置',
      // -- 文件面板 --
      file_name: '文件名',
      file_size: '大小',
      file_mtime: '修改时间',
      file_actions: '操作',
      btn_upload: '上传',
      btn_download: '下载',
      btn_delete: '删除',
      btn_rename: '重命名',
      btn_mkdir: '新建目录',
      btn_refresh: '刷新',
      // -- 通用 --
      btn_ok: '确定',
      btn_cancel: '取消',
      btn_close: '关闭',
      status_loading: '加载中…',
      status_empty: '暂无数据',
      status_error: '请求失败',
      // -- 卷 --
      vol_name: '卷名',
      vol_type: '类型',
      vol_capacity: '容量',
      // -- 通知 --
      notify_title: '通知',
      notify_test: '测试通知',
      // -- 语言 --
      lang_switch: '语言',
    },
    en: {
      upload_files: 'Upload Files',
      confirm_delete: 'Delete "{name}"?',
      search_placeholder: 'Search files…',
      nav_files: 'Files',
      nav_transfer: 'Transfer',
      nav_volumes: 'Volumes',
      nav_hub: 'Hub',
      nav_notify: 'Notifications',
      nav_audit: 'Audit',
      nav_settings: 'Settings',
      file_name: 'Name',
      file_size: 'Size',
      file_mtime: 'Modified',
      file_actions: 'Actions',
      btn_upload: 'Upload',
      btn_download: 'Download',
      btn_delete: 'Delete',
      btn_rename: 'Rename',
      btn_mkdir: 'New Folder',
      btn_refresh: 'Refresh',
      btn_ok: 'OK',
      btn_cancel: 'Cancel',
      btn_close: 'Close',
      status_loading: 'Loading…',
      status_empty: 'No data',
      status_error: 'Request failed',
      vol_name: 'Name',
      vol_type: 'Type',
      vol_capacity: 'Capacity',
      notify_title: 'Notifications',
      notify_test: 'Test Notification',
      lang_switch: 'Language',
    },
  };

  function normalize(lang) {
    return lang === 'en' ? 'en' : 'zh';
  }

  // lang() 解析当前语言：localStorage > navigator.language > 默认 zh。
  function lang() {
    try {
      if (typeof localStorage !== 'undefined' && localStorage) {
        const saved = localStorage.getItem(STORAGE_KEY);
        if (saved) return normalize(saved);
      }
      if (typeof navigator !== 'undefined' && navigator.language) {
        const nl = String(navigator.language).toLowerCase();
        if (nl.startsWith('zh')) return 'zh';
        if (nl.startsWith('en')) return 'en';
        return 'en';
      }
    } catch (e) {
      /* ignore */
    }
    return 'zh';
  }

  // t() 取词：当前语言命中 → 值；缺 key 回落 zh；两语言均缺返回 key 原文。
  function t(key, vars) {
    const cur = dicts[lang()] || {};
    let v = cur[key];
    if (v === undefined && dicts.zh) v = dicts.zh[key];
    if (v === undefined) v = key;
    return vars ? fmt(v, vars) : v;
  }

  // fmt() 插值：{name} 替换；缺变量保留原文。
  function fmt(tpl, vars) {
    if (!tpl || !vars) return tpl;
    return String(tpl).replace(/\{(\w+)\}/g, (m, name) =>
      Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m,
    );
  }

  // setLang() 切换语言：持久化 localStorage + html lang 同步 + 派发 i18n:changed。
  function setLang(newLang) {
    const l = normalize(newLang);
    try {
      if (typeof localStorage !== 'undefined' && localStorage) {
        localStorage.setItem(STORAGE_KEY, l);
      }
      if (typeof document !== 'undefined' && document.documentElement) {
        document.documentElement.lang = l;
      }
    } catch (e) {
      /* ignore */
    }
    if (typeof window !== 'undefined' && window.dispatchEvent) {
      window.dispatchEvent(new CustomEvent('i18n:changed', { detail: { lang: l } }));
    }
    return l;
  }

  // applyStaticI18n() 批量替换 data-i18n 文本与 data-i18n-placeholder。
  function applyStaticI18n() {
    if (typeof document === 'undefined' || !document.querySelectorAll) return;
    document.querySelectorAll('[data-i18n]').forEach((el) => {
      const key = el.getAttribute('data-i18n');
      if (key) el.textContent = t(key);
    });
    document.querySelectorAll('[data-i18n-placeholder]').forEach((el) => {
      const key = el.getAttribute('data-i18n-placeholder');
      if (key) el.setAttribute('placeholder', t(key));
    });
  }

  global.I18N = { dicts, t, fmt, lang, setLang, applyStaticI18n };
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { dicts, t, fmt, lang, setLang, applyStaticI18n };
  }
})(typeof globalThis !== 'undefined' ? globalThis : this);
