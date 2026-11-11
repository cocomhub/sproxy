// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// issues.js WebUI「待处理事项」渲染纯函数（用户裁定 2026-11-10：启动发现的残留/失败必须能被人工看到）。
// 服务端：GET /api/issues（列表 + stats）、GET /api/issues/detail?id=、POST /api/issues/resolve?id=&note=、
// POST /api/issues/scan。纯函数可 node --test，不依赖 DOM。

'use strict';

// 状态 → 中文标签（与 pkg/server/issues 的 Status 取值对齐）。
const ISSUE_STATUS_LABELS = {
  open: '待处理',
  retrying: '重试中',
  needs_human: '需人工接入',
  resolved: '已解决',
};

// 状态 → 颜色（needs_human 必须最醒目：这正是"必须能人工感知"的那一类）。
const ISSUE_STATUS_COLORS = {
  open: 'var(--text-secondary)',
  retrying: '#b8860b',
  needs_human: '#c0392b',
  resolved: 'var(--text-muted)',
};

function issueStatusLabel(status) {
  return ISSUE_STATUS_LABELS[status] || status || '-';
}

function issueStatusColor(status) {
  return ISSUE_STATUS_COLORS[status] || 'var(--text-primary)';
}

// issuesStatsHtml(stats) → 状态汇总条（未解决优先展示）。
function issuesStatsHtml(stats) {
  const s = stats || {};
  const order = ['needs_human', 'open', 'retrying', 'resolved'];
  const parts = order.map(function (k) {
    const n = s[k] || 0;
    return '<span style="margin-right:14px;color:' + issueStatusColor(k) + ';">' +
      issuesEscHtml(issueStatusLabel(k)) + '：<b>' + n + '</b></span>';
  });
  const total = order.reduce(function (a, k) { return a + (s[k] || 0); }, 0);
  if (total === 0) {
    return '<div style="font-size:13px;color:var(--text-muted);">暂无待处理事项（存储审查未发现差异）</div>';
  }
  return '<div style="font-size:13px;">' + parts.join('') + '</div>';
}

// issuesFiltersHtml(cur) → 筛选控件（状态/卷/仅未解决）。cur = {status, volume, unresolved}
function issuesFiltersHtml(cur) {
  const c = cur || {};
  const opts = ['', 'needs_human', 'open', 'retrying', 'resolved'].map(function (v) {
    const sel = (c.status || '') === v ? ' selected' : '';
    const label = v === '' ? '全部状态' : issueStatusLabel(v);
    return '<option value="' + issuesEscHtml(v) + '"' + sel + '>' + issuesEscHtml(label) + '</option>';
  }).join('');
  const checked = c.unresolved ? ' checked' : '';
  return '<div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:8px 0;">' +
    '<select id="issues-status" style="padding:5px 8px;font-size:12px;">' + opts + '</select>' +
    '<input id="issues-volume" placeholder="卷名（可选）" value="' + issuesEscHtml(c.volume || '') + '" style="padding:5px 8px;font-size:12px;">' +
    '<label style="font-size:12px;"><input type="checkbox" id="issues-unresolved"' + checked + '> 只看未解决</label>' +
    '<button type="button" id="issues-refresh-btn" class="btn btn-sm">刷新</button>' +
    '<button type="button" id="issues-scan-btn" class="btn btn-sm btn-primary">立即审查存储</button>' +
    '<span id="issues-msg" style="font-size:12px;color:var(--text-muted);"></span></div>';
}

// issuesListHtml(data) → 事项列表表格（每行可展开详情 + 人工解决按钮）。
// data = {total, stats, items:[Item]}
function issuesListHtml(data) {
  const items = (data && data.items) || [];
  if (items.length === 0) {
    return '<div class="empty-msg">未发现待处理事项</div>';
  }
  const rows = items.map(function (it) {
    const retry = it.retryable ? '可自动重试' : '需人工决策';
    return '<tr>' +
      '<td style="padding:6px 8px;color:' + issueStatusColor(it.status) + ';white-space:nowrap;">' +
      issuesEscHtml(issueStatusLabel(it.status)) + '</td>' +
      '<td style="padding:6px 8px;font-family:monospace;font-size:12px;">' + issuesEscHtml(it.kind) + '</td>' +
      '<td style="padding:6px 8px;">' + issuesEscHtml(it.volume) + '</td>' +
      '<td style="padding:6px 8px;font-family:monospace;font-size:12px;word-break:break-all;">' + issuesEscHtml(it.path) + '</td>' +
      '<td style="padding:6px 8px;font-size:12px;color:var(--text-secondary);">' + issuesEscHtml(retry) +
      (it.attempts ? '（已试 ' + it.attempts + ' 次）' : '') + '</td>' +
      '<td style="padding:6px 8px;font-size:12px;">' + issuesEscHtml(it.detail) +
      (it.last_error ? '<div style="color:#c0392b;">最近失败：' + issuesEscHtml(it.last_error) + '</div>' : '') + '</td>' +
      '<td style="padding:6px 8px;white-space:nowrap;">' +
      '<button type="button" class="btn btn-sm issues-resolve-btn" data-id="' + issuesEscHtml(it.id) + '">标记已解决</button>' +
      '</td></tr>';
  }).join('');
  return '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr style="background:var(--bg-hover);">' +
    '<th style="padding:6px 8px;text-align:left;">状态</th>' +
    '<th style="padding:6px 8px;text-align:left;">类型</th>' +
    '<th style="padding:6px 8px;text-align:left;">卷</th>' +
    '<th style="padding:6px 8px;text-align:left;">位置</th>' +
    '<th style="padding:6px 8px;text-align:left;">重试</th>' +
    '<th style="padding:6px 8px;text-align:left;">详细原因（人工判断依据）</th>' +
    '<th style="padding:6px 8px;text-align:left;">操作</th></tr></thead><tbody>' + rows + '</tbody></table>';
}

// issuesPanelHtml(data) → 完整面板（汇总 + 筛选 + 列表）。
function issuesPanelHtml(data, filters) {
  return issuesStatsHtml(data && data.stats) + issuesFiltersHtml(filters) + issuesListHtml(data);
}

// issuesDetailHtml(item) → 单条详情（含解决记录，供人工核对历史上怎么处理的）。
function issuesDetailHtml(item) {
  if (!item) {
    return '<div class="empty-msg">未找到该事项</div>';
  }
  const row = function (k, v) {
    return '<div style="margin:4px 0;"><span style="color:var(--text-muted);">' + issuesEscHtml(k) + '：</span>' + issuesEscHtml(v) + '</div>';
  };
  return '<div style="font-size:13px;">' +
    row('ID', item.id) + row('状态', issueStatusLabel(item.status)) + row('类型', item.kind) +
    row('卷', item.volume) + row('位置', item.path) + row('来源', item.source) +
    row('详细原因', item.detail) + row('最近失败', item.last_error || '-') +
    row('重试次数', String(item.attempts || 0)) + row('出现次数', String(item.occurrences || 0)) +
    row('解决说明', item.resolution || '-') + '</div>';
}

// issuesResolveBody(note) → 人工解决请求（服务端要求 note 必填：写明处置方式，供审计）。
function issuesResolveQuery(id, note) {
  return '/api/issues/resolve?id=' + encodeURIComponent(id || '') + '&note=' + encodeURIComponent(note || '');
}

// issuesListQuery(filters) → 列表请求 URL（筛选为空时不带参数）。
function issuesListQuery(filters) {
  const f = filters || {};
  const q = [];
  if (f.status) { q.push('status=' + encodeURIComponent(f.status)); }
  if (f.volume) { q.push('volume=' + encodeURIComponent(f.volume)); }
  if (f.kind) { q.push('kind=' + encodeURIComponent(f.kind)); }
  if (f.unresolved) { q.push('unresolved=1'); }
  return '/api/issues' + (q.length ? '?' + q.join('&') : '');
}

function issuesEscHtml(s) {
  return String(s === undefined || s === null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// 浏览器全局导出（node --test 下 require 使用 module.exports）。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    issueStatusLabel: issueStatusLabel,
    issueStatusColor: issueStatusColor,
    issuesStatsHtml: issuesStatsHtml,
    issuesFiltersHtml: issuesFiltersHtml,
    issuesListHtml: issuesListHtml,
    issuesPanelHtml: issuesPanelHtml,
    issuesDetailHtml: issuesDetailHtml,
    issuesResolveQuery: issuesResolveQuery,
    issuesListQuery: issuesListQuery,
    escHtml: issuesEscHtml,
  };
}
