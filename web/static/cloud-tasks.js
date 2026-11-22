// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cloud-tasks.js WebUI「云端下载任务」渲染纯函数（2026-11-21）。
//
// 背景：此前任务失败后从列表消失（1h TTL）、看不到失败类别与部分产物、也无法一键重试。
// 服务端已提供：GET /api/cloud/tasks（含 diagnostics）、POST /api/cloud/tasks/retry、
// POST /api/cloud/tasks/{id}/cancel、DELETE /api/cloud/tasks/{id}。
// 本文件只做纯函数（可 node --test）与轻量渲染，不引入框架。

'use strict';

// 状态 → 中文标签（与 pkg/cloud 的 Status 取值对齐）。
const CT_STATUS_LABELS = {
  pending: '排队中',
  downloading: '下载中',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
};
const CT_STATUS_COLORS = {
  pending: 'var(--text-secondary)',
  downloading: '#1f6feb',
  completed: '#2da44e',
  failed: '#c0392b',
  cancelled: 'var(--text-muted)',
};

// 失败归类 → 中文说明 + 是否"可自动重试"（与服务端 classifyCloudError 的 7 类对齐）。
const CT_CLASS_INFO = {
  source_unreachable: { label: '源不可达', retryable: true, hint: '稍后自动重试；仍失败可换源' },
  throttled: { label: '源限流', retryable: true, hint: '退避后自动重试' },
  content_mismatch: { label: '内容不一致', retryable: true, hint: '会自动换源重试' },
  target_conflict: { label: '目标冲突', retryable: false, hint: '需人工决定是否覆盖' },
  disk_full: { label: '空间不足', retryable: false, hint: '需扩容或清理' },
  auth: { label: '认证失败', retryable: false, hint: '需修正凭据' },
  unknown: { label: '未知', retryable: false, hint: '查看错误详情' },
};

function ctStatusLabel(status) {
  return CT_STATUS_LABELS[status] || status || '-';
}
function ctStatusColor(status) {
  return CT_STATUS_COLORS[status] || 'var(--text-secondary)';
}

// ctClassInfo 返回失败归类说明（未知类别按 unknown 处理，绝不吞掉原因）。
function ctClassInfo(cls) {
  return CT_CLASS_INFO[cls] || CT_CLASS_INFO.unknown;
}

// ctProgressPercent 计算进度百分比（0–100；未知总量返回 null，UI 显示 '-'）。
function ctProgressPercent(t) {
  if (!t) return null;
  const total = Number(t.total_size || 0);
  const done = Number(t.downloaded || (t.diagnostics && t.diagnostics.partial_size) || 0);
  if (!(total > 0) || done <= 0) return total > 0 ? 0 : null;
  const pct = Math.floor((done / total) * 100);
  return pct > 100 ? 100 : pct;
}

// ctStageLabel 阶段标签（服务端 diagnostics.stage）。
function ctStageLabel(stage) {
  switch (stage) {
    case 'resolving': return '解析中';
    case 'downloading': return '下载中';
    case 'verifying': return '校验中';
    case 'transferring': return '转存中';
    default: return stage || '-';
  }
}

// ctFilterTasks 按状态/是否仅看失败过滤（未指定 = 全量）。
function ctFilterTasks(tasks, filters) {
  const list = Array.isArray(tasks) ? tasks : [];
  const f = filters || {};
  const q = String(f.query || '').trim().toLowerCase();
  return list.filter(function (t) {
    if (!t) return false;
    if (f.status && t.status !== f.status) return false;
    if (f.failedOnly && t.status !== 'failed' && t.status !== 'cancelled') return false;
    if (f.volume && (!t.transfer || t.transfer.volume !== f.volume)) return false;
    if (f.cls) {
      const cls = (t.diagnostics && t.diagnostics.last_error_class) || 'unknown';
      if (cls !== f.cls) return false;
    }
    if (q) {
      // 关键字匹配：文件名的子串 或 转存路径子串（排障时最常用两种定位）。
      const name = String(t.filename || '').toLowerCase();
      const path = String((t.transfer && t.transfer.path) || '').toLowerCase();
      if (name.indexOf(q) === -1 && path.indexOf(q) === -1) return false;
    }
    return true;
  });
}

// ctPageRange 计算分页区间（总数为 0 时返回 [0,0]；越界自动夹取）。
// 服务端 /api/cloud/tasks 支持 offset/limit，UI 用固定页大小避免一次拉上千条。
function ctPageRange(offset, limit, total) {
  const t = Math.max(0, Number(total) || 0);
  const lim = Math.max(1, Number(limit) || 100);
  // 越界夹到**最后一页起点**（而非 t-1），保证"末页"语义与分页按钮一致。
  const lastStart = t === 0 ? 0 : Math.max(0, Math.floor((t - 1) / lim) * lim);
  const off = Math.min(Math.max(0, Number(offset) || 0), lastStart);
  const end = Math.min(off + lim, t);
  return [off, end];
}

// ctExportTSV 把任务清单导出为 TSV（表头 + 每行一条），便于换环境复跑/工单留档。
// 列：id / status / stage / error_class / attempts / volume / path / url / partial_bytes
function ctExportTSV(tasks) {
  const head = ['id', 'status', 'stage', 'error_class', 'attempts', 'volume', 'path', 'url', 'partial_bytes'];
  const rows = (Array.isArray(tasks) ? tasks : []).map(function (t) {
    const d = t.diagnostics || {};
    const tr = t.transfer || {};
    return [
      t.id || '', t.status || '', d.stage || '', d.last_error_class || '', String(d.attempts || 0),
      tr.volume || '', tr.path || '', t.url || '', String(d.partial_size || 0),
    ].join(String.fromCharCode(9));
  });
  return head.join(String.fromCharCode(9)) + String.fromCharCode(10) + rows.join(String.fromCharCode(10)) + (rows.length ? String.fromCharCode(10) : '');
}

// ctParseTSV 解析导出的任务清单（表头驱动；容错缺列/空行，未知列忽略）。
// 返回 {rows: [{id,status,volume,path,url,error_class,attempts}], skipped: n}。
// 设计：只依赖**表头名**，因此新增列不会破坏旧文件的导入。
function ctParseTSV(text) {
  const lines = String(text || '').split(String.fromCharCode(10)).filter(function (l) { return l.trim() !== ''; });
  if (lines.length < 2) return { rows: [], skipped: 0 };
  const head = lines[0].split(String.fromCharCode(9));
  const idx = {};
  head.forEach(function (h, i) { idx[h.trim()] = i; });
  if (idx.id === undefined && idx.path === undefined) return { rows: [], skipped: lines.length - 1 };
  const rows = [];
  let skipped = 0;
  for (let i = 1; i < lines.length; i++) {
    const cols = lines[i].split(String.fromCharCode(9));
    const pick = function (k) { const j = idx[k]; return j === undefined ? '' : String(cols[j] || '').trim(); };
    const row = {
      id: pick('id'), status: pick('status'), volume: pick('volume'), path: pick('path'),
      url: pick('url'), error_class: pick('error_class'), attempts: Number(pick('attempts') || 0),
    };
    if (!row.id && !(row.volume && row.path)) { skipped++; continue; }
    rows.push(row);
  }
  return { rows: rows, skipped: skipped };
}

// ctImportRetryPayloads 由导入行生成重试请求体：
// 优先 task_id（最精确）；无 id 时用 transfer_volume + transfer_path 定位；
// 仅保留**失败/取消/未知状态**（其它状态无需重试）。两者都不具备的行跳过。
function ctImportRetryPayloads(rows) {
  const out = [];
  (Array.isArray(rows) ? rows : []).forEach(function (r) {
    if (!r) return;
    const st = String(r.status || '');
    if (st === 'completed' || st === 'downloading' || st === 'pending') return;
    if (r.id) { out.push({ task_id: String(r.id), force: true }); return; }
    if (r.volume && r.path) { out.push({ transfer_volume: String(r.volume), transfer_path: String(r.path), force: true }); }
  });
  return out;
}

// ctFailedLastHour 统计「最近 1 小时内失败」的任务数（用于概览卡片趋势副标题）。
// 依据 `diagnostics.last_error_at`（服务端失败时写入）；时间无法解析的计入 unknown → 不计。
function ctFailedLastHour(tasks, nowMs) {
  const now = Number(nowMs) || Date.now();
  const windowMs = 3600 * 1000;
  let n = 0;
  (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
    if (!t || t.status !== 'failed') return;
    const d = t.diagnostics || {};
    if (!d.last_error_at) return;
    const ts = Date.parse(d.last_error_at);
    if (isNaN(ts)) return;
    if (now - ts >= 0 && now - ts <= windowMs) n++;
  });
  return n;
}

// ctVolumes 列出任务涉及的转存卷（供筛选下拉；稳定排序、去空）。
function ctVolumes(tasks) {
  const set = {};
  (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
    const v = t && t.transfer && t.transfer.volume;
    if (v) set[v] = true;
  });
  return Object.keys(set).sort();
}

// ctCopyText 返回「复制」按钮应写入剪贴板的文本（任务 ID + 源 + 目标，便于贴到工单/命令行）。
function ctCopyText(t) {
  if (!t) return '';
  const target = t.transfer ? (t.transfer.volume || '') + ':' + (t.transfer.path || '') : '';
  return [t.id || '', t.url || '', target].filter(function (x) { return x; }).join(String.fromCharCode(10));
}

// ctSummarize 汇总数量（供面板顶部显示）。
function ctSummarize(tasks) {
  const out = { total: 0, running: 0, completed: 0, failed: 0, cancelled: 0 };
  (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
    if (!t) return;
    out.total++;
    if (t.status === 'pending' || t.status === 'downloading') out.running++;
    else if (t.status === 'completed') out.completed++;
    else if (t.status === 'failed') out.failed++;
    else if (t.status === 'cancelled') out.cancelled++;
  });
  return out;
}

// ctRetryPayload 构造重试请求体（按任务 ID；force 可选）。
function ctRetryPayload(taskId, force) {
  return { task_id: String(taskId || ''), force: !!force };
}

// ctBatchRetryPayloads 由选中的任务 ID 生成**逐条**重试请求体（服务端按单任务处理）。
// 为什么逐条：服务端 retry 语义是"选择器 → 选中任务"，批量已在 CLI `batch` 覆盖；
// UI 侧循环提交可让单条失败不影响其余（与 CLI `--continue-on-error` 一致）。
function ctBatchRetryPayloads(ids) {
  return (Array.isArray(ids) ? ids : [])
    .filter(function (id) { return !!id; })
    .map(function (id) { return ctRetryPayload(id, true); });
}

// ctBatchCancelTargets 由勾选 ID 与任务列表筛出**可取消**的任务（pending/downloading）。
// 已完成/失败/取消的行不产生取消请求（否则会得到无意义 4xx）。
function ctBatchCancelTargets(tasks, ids) {
  const want = {};
  (Array.isArray(ids) ? ids : []).forEach(function (id) { if (id) want[id] = true; });
  return (Array.isArray(tasks) ? tasks : []).filter(function (t) {
    return t && want[t.id] && (t.status === 'pending' || t.status === 'downloading');
  }).map(function (t) { return t.id; });
}

// ctFailuresOnly 选出可重试任务（失败/取消）。
function ctFailuresOnly(tasks) {
  return (Array.isArray(tasks) ? tasks : []).filter(function (t) {
    return t && (t.status === 'failed' || t.status === 'cancelled');
  });
}

// ctDetailHtml 渲染单任务详情（展开行）：阶段/尝试次数/失败原文/部分产物路径。
// 目的：面板列表只给摘要，**排障需要原文与产物位置**（此前只能看 CLI 或日志）。
function ctDetailHtml(t) {
  if (!t) return '';
  const d = t.diagnostics || {};
  const rows = [
    ['任务 ID', t.id || '-'],
    ['源 URL', t.url || '-'],
    ['转存目标', t.transfer ? (t.transfer.volume || '') + ':' + (t.transfer.path || '') : '-'],
    ['阶段', ctStageLabel(d.stage)],
    ['失败次数', String(d.attempts || 0)],
    ['失败类别', ctClassInfo(d.last_error_class).label + '（' + ctClassInfo(d.last_error_class).hint + '）'],
    ['失败原文', d.last_error || '-'],
    ['部分产物', d.partial_path ? d.partial_path + '（' + Math.round((d.partial_size || 0) / 1048576) + ' MiB）' : '-'],
  ];
  let html = '<tr class="ct-detail" data-detail-for="' + ctEsc(t.id) + '"><td colspan="7" ' +
    'style="background:rgba(127,127,127,.06);padding:10px 12px;"><table style="font-size:12px;">';
  rows.forEach(function (r) {
    html += '<tr><td style="color:var(--text-muted);padding:2px 12px 2px 0;white-space:nowrap;">' + ctEsc(r[0]) +
      '</td><td style="padding:2px 0;word-break:break-all;">' + ctEsc(r[1]) + '</td></tr>';
  });
  return html + '</table></td></tr>';
}

// ctGroupByClass 失败任务按类别分组（用于"哪类失败最多"的一眼判断）。
function ctGroupByClass(tasks) {
  const out = {};
  (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
    if (!t || t.status !== 'failed') return;
    const cls = (t.diagnostics && t.diagnostics.last_error_class) || 'unknown';
    out[cls] = (out[cls] || 0) + 1;
  });
  return out;
}

function ctEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// ctSummaryHtml 顶部汇总（空列表给明确提示）。
function ctSummaryHtml(tasks) {
  const s = ctSummarize(tasks);
  if (s.total === 0) return '<div style="color:var(--text-secondary);font-size:13px;">暂无云端下载任务</div>';
  const cls = ctGroupByClass(tasks);
  const failParts = Object.keys(cls).map(function (k) {
    return ctClassInfo(k).label + ' ' + cls[k];
  });
  let html = '<div style="font-size:13px;margin-bottom:6px;">总 ' + s.total +
    '｜进行中 ' + s.running + '｜完成 ' + s.completed + '｜失败 ' + s.failed +
    '｜取消 ' + s.cancelled;
  if (failParts.length) html += '（' + ctEsc(failParts.join('，')) + '）';
  return html + '</div>';
}

// ctRowHtml 单行渲染：状态/进度/阶段/失败类别/尝试次数 + 操作按钮。
function ctRowHtml(t) {
  if (!t) return '';
  const pct = ctProgressPercent(t);
  const d = t.diagnostics || {};
  const failure = t.status === 'failed'
    ? ctEsc(ctClassInfo(d.last_error_class).label) + '（' + (d.attempts || 0) + ' 次）'
    : '-';
  const retryBtn = (t.status === 'failed' || t.status === 'cancelled')
    ? '<button class="ct-retry" data-id="' + ctEsc(t.id) + '" style="margin-right:6px;">重试</button>'
    : '';
  const cancelBtn = (t.status === 'pending' || t.status === 'downloading')
    ? '<button class="ct-cancel" data-id="' + ctEsc(t.id) + '">取消</button>'
    : '';
  const canRetry = (t.status === 'failed' || t.status === 'cancelled');
  return '<tr data-id="' + ctEsc(t.id) + '">' +
    '<td>' + (canRetry ? '<input type="checkbox" class="ct-pick" data-id="' + ctEsc(t.id) + '">' : '') + '</td>' +
    '<td style="color:' + ctStatusColor(t.status) + ';">' + ctEsc(ctStatusLabel(t.status)) + '</td>' +
    '<td>' + ctEsc(t.filename || t.url || '') + '</td>' +
    '<td>' + (pct === null ? '-' : pct + '%') + '</td>' +
    '<td>' + ctEsc(ctStageLabel(d.stage)) + '</td>' +
    '<td>' + failure + '</td>' +
    '<td>' + ctEsc(d.partial_size ? (Math.round(d.partial_size / 1048576) + ' MiB') : '-') + '</td>' +
    '<td>' + retryBtn + cancelBtn +
      '<button class="ct-detail-btn" data-id="' + ctEsc(t.id) + '" style="margin-left:6px;">详情</button>' +
      '<button class="ct-copy" data-id="' + ctEsc(t.id) + '" style="margin-left:6px;">复制ID</button></td>' +
    '</tr>';
}

// ctTableHtml 整表渲染（含表头；空列表提示）。
function ctTableHtml(tasks) {
  const list = Array.isArray(tasks) ? tasks : [];
  if (list.length === 0) return '';
  let html = '<div style="margin-bottom:6px;font-size:12px;">' +
    '<label><input type="checkbox" id="ct-pick-all"> 全选失败</label> ' +
    '<button id="ct-retry-selected" style="margin-left:8px;">批量重试选中</button>' +
    '<button id="ct-cancel-selected" style="margin-left:6px;">批量取消选中</button>' +
    '<span id="ct-msg" style="margin-left:10px;color:var(--text-muted);"></span></div>' +
    '<table style="width:100%;font-size:12px;border-collapse:collapse;"><thead><tr>' +
    '<th style="width:24px;"></th><th>状态</th><th>文件</th><th>进度</th><th>阶段</th><th>失败原因</th><th>已下载</th><th>操作</th>' +
    '</tr></thead><tbody>';
  list.forEach(function (t) { html += ctRowHtml(t); });
  return html + '</tbody></table>';
}

// ctPagerHtml 分页控件（上一页/下一页 + 区间提示）。服务端支持 offset/limit。
function ctPagerHtml(total, offset, limit) {
  const lim = limit || 100;
  const off = offset || 0;
  const range = ctPageRange(off, lim, total);
  const prevDis = off <= 0 ? ' disabled' : '';
  const nextDis = range[1] >= total ? ' disabled' : '';
  return '<div style="display:flex;gap:8px;align-items:center;margin-top:8px;font-size:12px;">' +
    '<button id="ct-prev"' + prevDis + '>上一页</button>' +
    '<button id="ct-next"' + nextDis + '>下一页</button>' +
    '<span style="color:var(--text-muted);">第 ' + (range[0] + 1) + '–' + range[1] + ' 条 / 共 ' + total + '</span></div>';
}

// ctPanelHtml 面板整体（汇总 + 表格）。
function ctPanelHtml(tasks, filters) {
  const filtered = ctFilterTasks(tasks, filters);
  return ctSummaryHtml(tasks) + ctTableHtml(filtered);
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    ctStatusLabel: ctStatusLabel,
    ctStatusColor: ctStatusColor,
    ctClassInfo: ctClassInfo,
    ctProgressPercent: ctProgressPercent,
    ctStageLabel: ctStageLabel,
    ctFilterTasks: ctFilterTasks,
    ctVolumes: ctVolumes,
    ctPageRange: ctPageRange,
    ctExportTSV: ctExportTSV,
    ctFailedLastHour: ctFailedLastHour,
    ctParseTSV: ctParseTSV,
    ctImportRetryPayloads: ctImportRetryPayloads,
    ctPagerHtml: ctPagerHtml,
    ctCopyText: ctCopyText,
    ctSummarize: ctSummarize,
    ctGroupByClass: ctGroupByClass,
    ctRetryPayload: ctRetryPayload,
    ctBatchRetryPayloads: ctBatchRetryPayloads,
    ctFailuresOnly: ctFailuresOnly,
    ctBatchCancelTargets: ctBatchCancelTargets,
    ctDetailHtml: ctDetailHtml,
    ctSummaryHtml: ctSummaryHtml,
    ctRowHtml: ctRowHtml,
    ctTableHtml: ctTableHtml,
    ctPanelHtml: ctPanelHtml,
  };
}

// ---- 浏览器侧交互（仅浏览器执行；node --test 下无 document，自动跳过） ----
if (typeof document !== 'undefined' && typeof window !== 'undefined') {
  // setCtMsg 记录操作结果消息（重渲染后仍显示：写入 __ctMsg，渲染时回填）。
  function setCtMsg(text) {
    window.__ctMsg = text || '';
    const el = document.getElementById('ct-msg');
    if (el) el.textContent = window.__ctMsg;
  }
  // ctFetchTasks 拉取任务列表（服务端 GET /api/cloud/tasks）。
  async function ctFetchTasks() {
    const res = await window.sclientTransport.coreRequest('GET', '/api/cloud/tasks', {});
    const data = window.sclientUtil.decodeJSON(res.body);
    return Array.isArray(data) ? data : (data.tasks || []);
  }

  // showCloudTasks 渲染面板并绑定操作（重试/取消/刷新/自动刷新）。
  // 为什么需要（2026-11-21）：失败任务此前只能靠 CLI 排查；面板让"看+操作"在一处完成。
  window.showCloudTasks = async function showCloudTasks() {
    const panel = document.getElementById('cloud-tasks-panel');
    if (!panel) return;
    const failedOnly = document.getElementById('ct-failed-only');
    const autoEl = document.getElementById('ct-auto');
    // 过滤状态以模块状态为**唯一真源**（2026-11-22）：卡片跳转会预置过滤，
    // 若仍从 DOM 读，则首次渲染覆盖预置值（实测 failedOnly 丢失）。
    const st = window.__ctFilterState || (window.__ctFilterState =
      { failedOnly: false, volume: '', query: '', cls: '' });
    const filters = {
      failedOnly: !!st.failedOnly,
      volume: st.volume || '',
      query: st.query || '',
      cls: st.cls || '',
    };
    let tasks = [];
    try {
      tasks = await ctFetchTasks();
    } catch (e) {
    panel.innerHTML = '<div style="color:#c0392b;font-size:13px;">加载失败：' + ctEsc(e.message) + '</div>';
      return;
    }
    window.__ctLastTasks = tasks; // 详情行展开需要完整任务对象（纯渲染用）
    panel.innerHTML =
      '<div style="display:flex;gap:12px;align-items:center;margin-bottom:8px;font-size:13px;">' +
      '<label><input type="checkbox" id="ct-failed-only"' + (filters.failedOnly ? ' checked' : '') + '> 只看失败/取消</label>' +
      '<label>卷 <select id="ct-volume"><option value="">全部</option>' +
      ctVolumes(tasks).map(function (v) {
        return '<option value="' + ctEsc(v) + '"' + (filters.volume === v ? ' selected' : '') + '>' + ctEsc(v) + '</option>';
      }).join('') + '</select></label>' +
      '<label>失败类别 <select id="ct-class"><option value="">全部</option>' +
      ['source_unreachable', 'throttled', 'content_mismatch', 'target_conflict', 'disk_full', 'auth', 'unknown']
        .map(function (c) {
          return '<option value="' + c + '"' + (filters.cls === c ? ' selected' : '') + '>' + ctClassInfo(c).label + '</option>';
        }).join('') + '</select></label>' +
      '<label>关键字 <input id="ct-query" type="search" placeholder="文件名/转存路径" value="' + ctEsc(filters.query || '') + '" style="width:180px;"></label>' +
      '<label><input type="checkbox" id="ct-auto"> 自动刷新(5s)</label>' +
      '<button id="ct-refresh">刷新</button>' +
      '<button id="ct-export">导出TSV</button>' +
      '<label style="cursor:pointer;">导入TSV重试<input id="ct-import" type="file" accept=".tsv,text/tab-separated-values" style="display:none;"></label>' +
      '</div>' + (function renderPaged() {
        // 分页在**过滤之后**做（过滤是客户端全量语义，服务端未提供卷/类别过滤）。
        const filtered = ctFilterTasks(tasks, filters);
        const limit = 100;
        const [start, end] = ctPageRange(window.__ctOffset || 0, limit, filtered.length);
        window.__ctOffset = start;
        return ctSummaryHtml(tasks) + ctTableHtml(filtered.slice(start, end)) +
          ctPagerHtml(filtered.length, start, limit);
      })();

    // 事件绑定（每次重渲染后重绑；控件是面板内新建的）。
    const importEl = document.getElementById('ct-import');
    if (importEl) importEl.addEventListener('change', function () {
      const f = importEl.files && importEl.files[0];
      if (!f) return;
      const reader = new FileReader();
      reader.onload = async function () {
        const parsed = ctParseTSV(String(reader.result || ''));
        const payloads = ctImportRetryPayloads(parsed.rows);
        const msg = document.getElementById('ct-msg');
        if (!payloads.length) {
          setCtMsg('导入文件中没有可重试的任务（已跳过 ' + parsed.skipped + ' 行）');
          return;
        }
        let ok = 0, failed = 0;
        for (const pl of payloads) {
          try {
            await window.sclientTransport.coreRequest('POST', '/api/cloud/tasks/retry', {
              bodyBytes: new TextEncoder().encode(JSON.stringify(pl)),
              headers: { 'Content-Type': 'application/json' },
            });
            ok++;
          } catch (e) { failed++; }
        }
        setCtMsg('导入重试：提交 ' + ok + ' 个，失败 ' + failed + ' 个，跳过 ' + parsed.skipped + ' 行');
        await window.showCloudTasks();
      };
      reader.readAsText(f);
    });
    const exportBtn = document.getElementById('ct-export');
    if (exportBtn) exportBtn.addEventListener('click', function () {
      // 导出**当前过滤结果**（与用户所见一致），便于复跑或贴工单。
      const list = ctFilterTasks(tasks, filters);
      const blob = new Blob([ctExportTSV(list)], { type: 'text/tab-separated-values' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'cloud-tasks.tsv';
      a.click();
      setTimeout(function () { URL.revokeObjectURL(a.href); }, 1000);
    });
    const refreshBtn = document.getElementById('ct-refresh');
    if (refreshBtn) refreshBtn.addEventListener('click', function () { void window.showCloudTasks(); });
    // 操作结果消息回填（必须在 innerHTML 渲染**之后**，否则 #ct-msg 还不存在）。
    if (window.__ctMsg) setCtMsg(window.__ctMsg);
    const syncHash = function () {
      if (typeof window.syncCloudFiltersHash === 'function') window.syncCloudFiltersHash();
    };
    const offsetReset = function () { window.__ctOffset = 0; syncHash(); };
    const prevBtn = document.getElementById('ct-prev');
    if (prevBtn) prevBtn.addEventListener('click', function () {
      window.__ctOffset = Math.max(0, (window.__ctOffset || 0) - 100);
      void window.showCloudTasks();
    });
    const nextBtn = document.getElementById('ct-next');
    if (nextBtn) nextBtn.addEventListener('click', function () {
      window.__ctOffset = (window.__ctOffset || 0) + 100;
      void window.showCloudTasks();
    });
    const clsSel = document.getElementById('ct-class');
    if (clsSel) clsSel.addEventListener('change', function () { void window.showCloudTasks(); });
    const volSel = document.getElementById('ct-volume');
    if (volSel) volSel.addEventListener('change', function () { void window.showCloudTasks(); });
    const qInput = document.getElementById('ct-query');
    if (qInput) {
      qInput.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') { st.query = qInput.value; offsetReset(); void window.showCloudTasks(); }
      });
      qInput.addEventListener('change', function () { st.query = qInput.value; offsetReset(); void window.showCloudTasks(); });
    }
    panel.querySelectorAll('.ct-copy').forEach(function (btn) {
      btn.addEventListener('click', async function () {
        const id = btn.getAttribute('data-id');
        const t = (window.__ctLastTasks || []).filter(function (x) { return x && x.id === id; })[0];
        try {
          await navigator.clipboard.writeText(ctCopyText(t));
          btn.textContent = '已复制';
          setTimeout(function () { btn.textContent = '复制ID'; }, 1200);
        } catch (e) {
          alert('复制失败：' + e.message);
        }
      });
    });

    const fOnly = document.getElementById('ct-failed-only');
    if (fOnly) fOnly.addEventListener('change', function () {
      st.failedOnly = !!fOnly.checked; window.__ctOffset = 0; syncHash(); void window.showCloudTasks();
    });

    // 详情展开/收起（行内插入 ct-detail 行；再次点击移除）。
    panel.querySelectorAll('.ct-detail-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        const id = btn.getAttribute('data-id');
        const exists = panel.querySelector('.ct-detail[data-detail-for="' + id + '"]');
        if (exists) { exists.remove(); return; }
        const row = btn.closest('tr');
        const t = (window.__ctLastTasks || []).filter(function (x) { return x && x.id === id; })[0];
        if (row && t) row.insertAdjacentHTML('afterend', ctDetailHtml(t));
      });
    });

    // 全选失败任务（勾选所有可重试行）。
    const pickAll = document.getElementById('ct-pick-all');
    if (pickAll) {
      pickAll.addEventListener('change', function () {
        panel.querySelectorAll('.ct-pick').forEach(function (cb) { cb.checked = !!pickAll.checked; });
      });
    }

    // 批量重试选中：逐条提交（单条失败不影响其余），完成后刷新并提示。
    const cancelSelBtn = document.getElementById('ct-cancel-selected');
    if (cancelSelBtn) cancelSelBtn.addEventListener('click', async function () {
      const ids = Array.prototype.slice.call(panel.querySelectorAll('.ct-pick:checked'))
        .map(function (cb) { return cb.getAttribute('data-id'); });
      const targets = ctBatchCancelTargets(tasks, ids);
      if (!targets.length) { setCtMsg('选中的任务都不可取消（仅排队中/下载中可取消）'); return; }
      let ok = 0, failed = 0;
      for (const id of targets) {
        try {
          await window.sclientTransport.coreRequest('POST',
            '/api/cloud/tasks/' + encodeURIComponent(id) + '/cancel', {});
          ok++;
        } catch (e) { failed++; }
      }
      setCtMsg('批量取消：成功 ' + ok + ' 个，失败 ' + failed + ' 个');
      await window.showCloudTasks();
    });

    const batchBtn = document.getElementById('ct-retry-selected');
    if (batchBtn) {
      batchBtn.addEventListener('click', async function () {
        const ids = Array.prototype.slice.call(panel.querySelectorAll('.ct-pick:checked'))
          .map(function (cb) { return cb.getAttribute('data-id'); });
        const msg = document.getElementById('ct-msg');
        if (!ids.length) { if (msg) msg.textContent = '先勾选要重试的任务'; return; }
        setCtMsg('重试中（' + ids.length + '）...');
        let ok = 0, failed = 0;
        for (const payload of ctBatchRetryPayloads(ids)) {
          try {
            await window.sclientTransport.coreRequest('POST', '/api/cloud/tasks/retry', {
              bodyBytes: new TextEncoder().encode(JSON.stringify(payload)),
              headers: { 'Content-Type': 'application/json' },
            });
            ok++;
          } catch (e) { failed++; }
        }
        setCtMsg('已提交 ' + ok + ' 个，失败 ' + failed + ' 个');
        await window.showCloudTasks();
      });
    }

    panel.querySelectorAll('.ct-retry').forEach(function (btn) {
      btn.addEventListener('click', async function () {
        const id = btn.getAttribute('data-id');
        try {
          await window.sclientTransport.coreRequest('POST', '/api/cloud/tasks/retry', {
            bodyBytes: new TextEncoder().encode(JSON.stringify(ctRetryPayload(id, true))),
            headers: { 'Content-Type': 'application/json' },
          });
          await window.showCloudTasks();
        } catch (e) {
          alert('重试失败：' + e.message);
        }
      });
    });
    panel.querySelectorAll('.ct-cancel').forEach(function (btn) {
      btn.addEventListener('click', async function () {
        const id = btn.getAttribute('data-id');
        try {
          await window.sclientTransport.coreRequest('POST',
            '/api/cloud/tasks/' + encodeURIComponent(id) + '/cancel', {});
          await window.showCloudTasks();
        } catch (e) {
          alert('取消失败：' + e.message);
        }
      });
    });

    // 自动刷新：勾选后每 5s 重渲染；离开面板（display:none）自动停，避免后台空转。
    if (autoEl) {
      autoEl.addEventListener('change', function () {
        if (!autoEl.checked) return;
        const timer = setInterval(function () {
          const p = document.getElementById('cloud-tasks-panel');
          if (!p || p.style.display === 'none') { clearInterval(timer); return; }
          void window.showCloudTasks();
        }, 5000);
      });
    }
  };
}
