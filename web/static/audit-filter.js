// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-filter.js — 审计面板的**筛选纯函数**（S4：审计新字段/筛选，设计
// docs/designs/2026-10-19-audit-wal-program.md「S4 面与文档」）。
//
// 设计约束：
//   - **纯函数、无 DOM / 无网络** ⇒ 可用 `node --test` 覆盖（R10 硬要求：改动的前端逻辑必须有单测）；
//   - 筛选在**客户端**做（面板一次拉最近 200 条），不额外打接口 ⇒ 不改变读面权限语义
//     （行已在服务端按单轴 Owner 过滤/脱敏，见设计 §5.7）。
//
// 字段语义（与 pkg/audit.Entry 对齐）：
//   - action / owner：**子串**匹配（不区分大小写，便于按前缀快速缩小范围）；
//   - result / kind：**精确**匹配（都是有限枚举值，模糊匹配无意义）；
//   - 空字符串 ⇒ 该维度不过滤。

/**
 * 创建空筛选条件（UI 初值）。
 * @returns {{action:string, owner:string, result:string, kind:string, volume:string}}
 */
function auditFilterEmpty() {
  // volume（评审 R13）：卷维度，服务端已写入 Entry.Volume 并经读面 JSON 暴露。
  return { action: '', owner: '', result: '', kind: '', volume: '' };
}

/**
 * 归一化：null/undefined ⇒ ''，统一小写（仅用于比较，不用于展示）。
 * @param {*} v
 * @returns {string}
 */
function auditFilterNorm(v) {
  return String(v === null || v === undefined ? '' : v).trim().toLowerCase();
}

/**
 * 是否存在任一生效的筛选维度（UI 用来决定是否显示「已筛选」提示）。
 * @param {{action?:string, owner?:string, result?:string, kind?:string}} filter
 * @returns {boolean}
 */
function auditFilterIsActive(filter) {
  const f = filter || {};
  return ['action', 'owner', 'result', 'kind', 'volume'].some(function (k) {
    return auditFilterNorm(f[k]) !== '';
  });
}

/**
 * 按条件筛选审计事件（保序；不修改入参）。
 * @param {Array<object>} events
 * @param {{action?:string, owner?:string, result?:string, kind?:string}} filter
 * @returns {Array<object>}
 */
function auditFilterRows(events, filter) {
  const f = filter || {};
  const action = auditFilterNorm(f.action);
  const owner = auditFilterNorm(f.owner);
  const result = auditFilterNorm(f.result);
  const kind = auditFilterNorm(f.kind);
  const volume = auditFilterNorm(f.volume);
  const src = Array.isArray(events) ? events : [];
  return src.filter(function (e) {
    if (!e) return false;
    if (action && !auditFilterNorm(e.action).includes(action)) return false;
    if (owner && !auditFilterNorm(e.owner).includes(owner)) return false;
    if (result && auditFilterNorm(e.result) !== result) return false;
    if (kind && auditFilterNorm(e.kind) !== kind) return false;
    if (volume && !auditFilterNorm(e.volume).includes(volume)) return false;
    return true;
  });
}

/**
 * 生成面板标题旁的计数摘要。
 * @param {number} total 拉取到的事件总数
 * @param {number} shown 当前显示条数
 * @returns {string} 例：「共 200 条」/「显示 3 / 200 条」
 */
function auditFilterSummary(total, shown) {
  const t = Number(total) || 0;
  const s = Number(shown) || 0;
  if (s === t) return '共 ' + t + ' 条';
  return '显示 ' + s + ' / ' + t + ' 条';
}

// 浏览器全局导出（供 app.js 使用）；Node 下导出供 node --test 直接 require。
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    auditFilterEmpty: auditFilterEmpty,
    auditFilterIsActive: auditFilterIsActive,
    auditFilterRows: auditFilterRows,
    auditFilterSummary: auditFilterSummary,
    auditFilterNorm: auditFilterNorm,
  };
}
if (typeof globalThis !== 'undefined') {
  globalThis.auditFilter = {
    auditFilterEmpty: auditFilterEmpty,
    auditFilterIsActive: auditFilterIsActive,
    auditFilterRows: auditFilterRows,
    auditFilterSummary: auditFilterSummary,
    auditFilterNorm: auditFilterNorm,
  };
}
