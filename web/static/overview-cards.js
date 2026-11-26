// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// overview-cards.js 控制台「概览」卡片纯函数（2026-11-22）。
//
// 目的：进入控制台第一屏就能看到「有没有东西在等我处理」——云端下载失败数、
// 待处理事项数（含需人工）、进行中任务数，避免逐个分区翻。

'use strict';

// overviewCards 汇总卡片数据。
// cloud: {total, running, completed, failed, cancelled}（ctSummarize 输出）
// issues: {open, retrying, needs_human, resolved}（服务端 stats）
// overviewStorage 汇总存储水位（volumes: [{name, capacity, usage, allowed, state}]）。
// 只统计 capacity>0 的卷；返回 {capacity, usage, percent}（无容量信息 ⇒ percent=null）。
function overviewStorage(volumes) {
  let cap = 0, use = 0;
  (Array.isArray(volumes) ? volumes : []).forEach(function (v) {
    if (!v) return;
    const c = Number(v.capacity || 0);
    if (!(c > 0)) return;
    cap += c;
    use += Number(v.usage || 0);
  });
  if (cap <= 0) return { capacity: 0, usage: 0, percent: null };
  const pct = Math.round((use / cap) * 100);
  return { capacity: cap, usage: use, percent: pct > 100 ? 100 : pct };
}

function overviewCards(cloud, issues, failedLastHour, storage) {
  const c = cloud || {};
  const i = issues || {};
  const pendingIssues = Number(i.open || 0) + Number(i.retrying || 0);
  const needsHuman = Number(i.needs_human || 0);
  return [
    { key: 'cloud-failed', label: '云端任务失败', value: Number(c.failed || 0),
      tone: Number(c.failed || 0) > 0 ? 'bad' : 'ok',
      hint: '点「云端任务」查看/重试' +
        (Number(failedLastHour || 0) > 0 ? '；近 1 小时新增 ' + Number(failedLastHour) + ' 个' : '') },
    { key: 'cloud-running', label: '云端进行中', value: Number(c.running || 0), tone: 'info', hint: '下载/转存中' },
    { key: 'issues-pending', label: '待处理事项', value: pendingIssues, tone: pendingIssues > 0 ? 'warn' : 'ok', hint: '自动重试中或待处理' },
    { key: 'issues-human', label: '需人工接入', value: needsHuman, tone: needsHuman > 0 ? 'bad' : 'ok', hint: '自动重试已放弃' },
    { key: 'storage-used', label: '存储水位',
      value: (storage && storage.percent !== null && storage.percent !== undefined) ? Number(storage.percent) : 0,
      tone: (storage && storage.percent >= 90) ? 'bad' : (storage && storage.percent >= 75 ? 'warn' : 'ok'),
      hint: (storage && storage.percent !== null && storage.percent !== undefined)
        ? '已用 ' + (storage.percent) + '%（' + Math.round((storage.usage || 0) / 1073741824) + ' / ' +
          Math.round((storage.capacity || 0) / 1073741824) + ' GiB）'
        : '未提供容量信息',
      unit: '%' },
  ];
}

// overviewToneColor 卡片色调（坏=红、警告=黄、正常=绿、信息=蓝）。
function overviewToneColor(tone) {
  switch (tone) {
    case 'bad': return '#c0392b';
    case 'warn': return '#b8860b';
    case 'ok': return '#2da44e';
    default: return '#1f6feb';
  }
}

// overviewCardAction 把卡片映射为「跳转到哪个分区 + 带上什么过滤」。
//
// 目的（2026-11-22）：概览卡片不仅显示数字，还要**一键到现场**（失败→云端任务并只看失败；
// 待处理/需人工→事项面板并预置状态过滤），减少"看到红灯还要自己找地方"的摩擦。
function overviewCardAction(key) {
  switch (key) {
    case 'cloud-failed': return { tab: 'cloud-tasks', filters: { failedOnly: true } };
    case 'cloud-running': return { tab: 'cloud-tasks', filters: { status: 'downloading' } };
    case 'issues-pending': return { tab: 'issues', status: '' };
    case 'issues-human': return { tab: 'issues', status: 'needs_human' };
    default: return { tab: 'stats', filters: {} };
  }
}

// overviewAuditHtml 渲染「最近审计事件」快捷流（最多 5 条；空则提示）。
function overviewAuditHtml(entries) {
  const list = Array.isArray(entries) ? entries.slice(0, 5) : [];
  let html = '<div style="margin-top:6px;"><div style="font-size:13px;color:var(--text-secondary);margin-bottom:6px;">' +
    '最近审计事件</div>';
  if (list.length === 0) {
    return html + '<div style="font-size:12px;color:var(--text-muted);">暂无事件</div></div>';
  }
  html += '<ul style="margin:0;padding-left:18px;font-size:12px;line-height:1.7;">';
  list.forEach(function (e) {
    const ok = (e.result || '') !== 'error';
    html += '<li><span style="font-family:monospace;">' + (e.action || '-') + '</span> ' +
      '<span style="color:var(--text-muted);">' + (e.object || '') + '</span> ' +
      '<span style="color:' + (ok ? '#2da44e' : '#c0392b') + ';">' + (e.result || '') + '</span></li>';
  });
  return html + '</ul></div>';
}

// overviewCardsHtml 渲染卡片区（数值为 0 也显示，避免"看不见=以为没有"）。
function overviewCardsHtml(cloud, issues, failedLastHour, storage) {
  const cards = overviewCards(cloud, issues, failedLastHour, storage);
  let html = '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:10px;margin-bottom:14px;">';
  cards.forEach(function (c) {
    html += '<div class="overview-card" data-card="' + c.key + '" title="' + c.hint + '" ' +
      'style="border:1px solid var(--border);border-left:4px solid ' + overviewToneColor(c.tone) + ';' +
      'border-radius:6px;padding:10px 12px;background:var(--modal-bg);">' +
      '<div style="font-size:12px;color:var(--text-secondary);">' + c.label + '</div>' +
      '<div style="font-size:22px;font-weight:600;color:' + overviewToneColor(c.tone) + ';line-height:1.3;">' +
      c.value + (c.unit || '') + '</div></div>';
  });
  return html + '</div>';
}

/**
 * overviewRenderPlan 是概览卡片的**并发渲染门控**（纯函数，便于单测）。
 *
 * 语义（2026-11-27 修复）：渲染进行中再次触发时**不得丢弃**，必须置 pending 让调用方在
 * 本轮结束后重放一次。旧实现 `if (inFlight) return;` 直接丢弃 —— 若被丢弃的那次恰好是
 * 「打开统计面板」的触发（首屏渲染还在飞），卡片区就永远不渲染，用户看到概率性空白
 * （CI e2e 实测：服务端任务已 failed，20s 后卡片仍不可见）。
 *
 * 返回 { run, pending }：run=true 表示调用方应立即开始渲染；pending 为更新后的挂起标记。
 */
function overviewRenderPlan(inFlight, pending) {
  if (inFlight) {
    return { run: false, pending: true };
  }
  return { run: true, pending: false };
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    overviewCards: overviewCards,
    overviewToneColor: overviewToneColor,
    overviewCardsHtml: overviewCardsHtml,
    overviewStorage: overviewStorage,
    overviewCardAction: overviewCardAction,
    overviewAuditHtml: overviewAuditHtml,
    overviewRenderPlan: overviewRenderPlan,
  };
}
