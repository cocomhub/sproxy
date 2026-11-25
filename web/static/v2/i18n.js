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

  var ZH2EN = {
    "名称": "Name",
    "状态": "Status",
    "卷": "Volume",
    "大小": "Size",
    "修改时间": "Modified",
    "校验": "Checksum",
    "操作": "Actions",
    "任务": "Task",
    "进度": "Progress",
    "更新": "Updated",
    "类型": "Type",
    "模式": "Mode",
    "容量": "Capacity",
    "已用": "Used",
    "用量": "Usage",
    "路径": "Path",
    "详情": "Detail",
    "结果": "Result",
    "动作": "Action",
    "主体": "Actor",
    "对象": "Object",
    "对象 / 详情": "Object / Detail",
    "尝试": "Attempts",
    "最近": "Last seen",
    "服务": "Services",
    "质量": "Quality",
    "节点": "Node",
    "虚拟 IP": "Virtual IP",
    "时间": "Time",
    "渠道": "Channel",
    "失败": "Failed",
    "方向": "Direction",
    "组": "Group",
    "路径 / 说明": "Path / Detail",
    "面 / 角色": "Face / Role",
    "备注": "Notes",
    "SK 数": "SK count",
    "存活 SK": "Alive SK",
    "AccessKey / Secret": "AccessKey / Secret",
    "TOTP 登录": "TOTP login",
    "任务详情": "Task detail",
    "加密隧道": "Encrypted tunnel",
    "只读信息": "Read-only info",
    "接入指引": "Onboarding",
    "问题明细（只隔离不删除）": "Issues (quarantined, not deleted)",
    "回收站为空": "Trash is empty",
    "无数据": "No data",
    "暂无云端任务": "No cloud tasks",
    "暂无任务组": "No task groups",
    "暂无凭据": "No credentials",
    "暂无卷": "No volumes",
    "暂无同步任务": "No sync tasks",
    "暂无审计条目": "No audit entries",
    "暂无待处理事项": "No pending issues",
    "暂无通知记录": "No notifications",
    "没有可用卷": "No available volumes",
    "没有已注册节点": "No registered nodes",
    "没有用户卷": "No user volumes",
  };

  /** 静态表头/卡片标题/空态标题的中英替换（原始中文存 dataset.zh，可无损切回）。 */
  function applyTexts() {
    var en = lang() === 'en';
    Array.prototype.forEach.call(document.querySelectorAll('th, .card-title, .empty b'), function (n) {
      if (n.hasAttribute('data-i18n')) { return; }
      if (!n.dataset.zh) { n.dataset.zh = n.textContent; }
      var zh = n.dataset.zh.trim();
      var tr = ZH2EN[zh];
      if (tr) { n.textContent = en ? tr : n.dataset.zh; }
    });
  }

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
    applyTexts();
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
    module.exports = { t: t, lang: lang, DICT: DICT, ZH2EN: ZH2EN };
  }
})();
