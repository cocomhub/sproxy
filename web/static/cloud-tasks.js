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
    if (f.minSizeMB) {
      const mb = Number(t.total_size || 0) / 1048576;
      if (!(mb >= Number(f.minSizeMB))) return false;
    }
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

// ctSortTasks 排序任务（不改变入参；返回新数组）。
// key: status | name | updated | size；dir: 'asc' | 'desc'。
// 排序稳定性：同键值按 id 兜底，保证多次渲染顺序一致（避免列表"跳动"）。
function ctSortTasks(tasks, key, dir) {
  const list = (Array.isArray(tasks) ? tasks : []).slice();
  const desc = dir === 'desc';
  const rank = { downloading: 0, pending: 1, failed: 2, cancelled: 3, completed: 4 };
  const val = function (t) {
    switch (key) {
      case 'status': return rank[t.status] === undefined ? 9 : rank[t.status];
      case 'name': return String(t.filename || '');
      case 'size': return Number(t.total_size || 0);
      case 'updated': return Date.parse(t.updated_at || '') || 0;
      default: return 0;
    }
  };
  list.sort(function (a, b) {
    const va = val(a), vb = val(b);
    let c;
    if (typeof va === 'string' || typeof vb === 'string') c = String(va).localeCompare(String(vb));
    else c = va - vb;
    if (c === 0) c = String(a.id || '').localeCompare(String(b.id || ''));
    return desc ? -c : c;
  });
  return list;
}

// 操作审计流（2026-11-23）：记录面板内的人工动作（重试/取消/导入/批量），
// 存 sessionStorage（同标签页刷新仍在，换标签页清空——这是"现场记录"，不是服务端审计）。
const CT_AUDIT_KEY = 'sproxy_ct_actions';
const CT_AUDIT_CAP = 100;

// ctAppendAudit 追加一条操作记录并裁剪到上限（返回新数组，不改入参）。
function ctAppendAudit(log, entry, cap) {
  const list = (Array.isArray(log) ? log : []).slice();
  const e = entry || {};
  list.unshift({
    ts: e.ts || new Date().toISOString(),
    action: e.action || '',
    target: e.target || '',
    result: e.result || '',
    detail: e.detail || '',
  });
  const max = cap || CT_AUDIT_CAP;
  return list.slice(0, max);
}

// ctAuditHtml 渲染最近操作（最多 n 条；空则提示"暂无操作"）。
function ctAuditHtml(log, n) {
  const list = (Array.isArray(log) ? log : []).slice(0, n || 10);
  let html = '<div style="margin-top:10px;font-size:12px;"><div style="color:var(--text-secondary);margin-bottom:4px;">' +
    '最近操作（本页面）</div>';
  if (list.length === 0) return html + '<div style="color:var(--text-muted);">暂无操作</div></div>';
  html += '<ul style="margin:0;padding-left:18px;line-height:1.7;">';
  list.forEach(function (e) {
    const ok = (e.result || '') !== 'failed';
    html += '<li><span style="font-family:monospace;">' + ctEsc(e.ts) + '</span> ' +
      ctEsc(e.action) + (e.target ? ' → ' + ctEsc(e.target) : '') +
      (e.detail ? ' ' + ctEsc(e.detail) : '') +
      ' <span style="color:' + (ok ? '#2da44e' : '#c0392b') + ';">' + ctEsc(e.result) + '</span></li>';
  });
  return html + '</ul></div>';
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

// ctGroupByVolume 按「转存目标卷」分组汇总（total/failed/running/completed），按总数降序。
// 用途：多卷场景一眼看出"哪个卷上的任务出问题"（2026-11-23）。
function ctGroupByVolume(tasks) {
  const map = {};
  (Array.isArray(tasks) ? tasks : []).forEach(function (t) {
    if (!t) return;
    const vol = (t.transfer && t.transfer.volume) || '(无目标卷)';
    const g = map[vol] || (map[vol] = { volume: vol, total: 0, failed: 0, running: 0, completed: 0 });
    g.total++;
    if (t.status === 'failed' || t.status === 'cancelled') g.failed++;
    else if (t.status === 'pending' || t.status === 'downloading') g.running++;
    else if (t.status === 'completed') g.completed++;
  });
  return Object.keys(map).map(function (k) { return map[k]; })
    .sort(function (a, b) { return b.total - a.total || a.volume.localeCompare(b.volume); });
}

// ctGroupByVolumeHtml 渲染卷分组 chips（点击可按该卷过滤）。
function ctGroupByVolumeHtml(tasks) {
  const groups = ctGroupByVolume(tasks);
  if (groups.length === 0) return '';
  let html = '<div style="margin:4px 0 8px;font-size:12px;display:flex;flex-wrap:wrap;gap:6px;align-items:center;">' +
    '<span style="color:var(--text-secondary);">按卷：</span>';
  groups.forEach(function (g) {
    const bad = g.failed > 0;
    html += '<button class="ct-vol-chip" data-volume="' + ctEsc(g.volume) + '" ' +
      'style="border:1px solid ' + (bad ? '#c0392b' : 'var(--border)') + ';background:' +
      (bad ? 'rgba(192,57,43,.10)' : 'transparent') + ';color:' + (bad ? '#c0392b' : 'var(--text-secondary)') +
      ';border-radius:10px;padding:2px 9px;cursor:pointer;font-size:12px;" ' +
      'title="点按该卷过滤">' + ctEsc(g.volume) + ' ' + g.total +
      (bad ? '（失败 ' + g.failed + '）' : '') + '</button>';
  });
  return html + '</div>';
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

// ctClassRetryPayloads 选出指定失败类别的可重试任务并生成请求体。
// 用途：概览/汇总里点「源不可达 3」这类聚合，直接批量重试该类别（服务端 retry 时会自动换源，
// 见 P4 `rotateSource`）。只取 failed/cancelled，避免把运行中任务也重试。
function ctClassRetryPayloads(tasks, cls) {
  const want = cls || 'unknown';
  return (Array.isArray(tasks) ? tasks : []).filter(function (t) {
    if (!t || (t.status !== 'failed' && t.status !== 'cancelled')) return false;
    const c = (t.diagnostics && t.diagnostics.last_error_class) || 'unknown';
    return c === want;
  }).map(function (t) { return ctRetryPayload(t.id, true); });
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

// ctRateSeries 提取速率序列（B/s）：只取 >0 的点（首点/空闲点不计，避免曲线贴地）。
function ctRateSeries(samples) {
  const list = Array.isArray(samples) ? samples : [];
  const out = [];
  list.forEach(function (s) {
    if (!s) return;
    const r = Number(s.rate_bps || 0);
    if (r > 0) out.push(r);
  });
  return out;
}

// ctSparklinePath 生成速率折线的 SVG path（宽 w、高 h；点 <2 返回 ''）。
// 归一化到自身最大值（相对曲线：只表达趋势，绝对速率在标签里给）。
function ctSparklinePath(samples, w, h) {
  const rates = ctRateSeries(samples);
  if (rates.length < 2) return '';
  const width = Number(w) > 0 ? Number(w) : 160;
  const height = Number(h) > 0 ? Number(h) : 36;
  const mx = Math.max.apply(null, rates);
  if (!(mx > 0)) return '';
  const step = width / (rates.length - 1);
  return rates.map(function (r, i) {
    const x = (i * step).toFixed(1);
    const y = (height - (r / mx) * height).toFixed(1);
    return (i === 0 ? 'M' : 'L') + x + ' ' + y;
  }).join(' ');
}

// ctSparklineSvg 生成速率曲线 SVG（无数据 ⇒ 空串；调用方决定占位文案）。
function ctSparklineSvg(samples, w, h) {
  const d = ctSparklinePath(samples, w, h);
  if (!d) return '';
  const rates = ctRateSeries(samples);
  const mx = Math.max.apply(null, rates);
  const label = '速率曲线：' + rates.length + ' 点，峰值 ' + ctRateText(mx);
  return '<svg class="ct-spark" width="' + (Number(w) || 160) + '" height="' + (Number(h) || 36) +
    '" viewBox="0 0 ' + (Number(w) || 160) + ' ' + (Number(h) || 36) + '" role="img" aria-label="' +
    ctEsc(label) + '"><path d="' + d + '" fill="none" stroke="#1f6feb" stroke-width="1.5"/></svg>' +
    '<div style="font-size:11px;color:var(--text-muted);">峰值 ' + ctRateText(mx) + '（' + rates.length + ' 点）</div>';
}

// ctRateText 人类可读速率（B/s → KB/s / MB/s）。
function ctRateText(bps) {
  const n = Number(bps || 0);
  if (!(n > 0)) return '0 B/s';
  if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB/s';
  if (n >= 1024) return (n / 1024).toFixed(1) + ' KB/s';
  return Math.round(n) + ' B/s';
}

// CT_ACTIONS 是失败任务的处理动作（与服务端 cloud.TaskAction 对齐）。
const CT_ACTIONS = {
  retry_download: { label: '续传下载', hint: '保留已下载部分，从断点继续' },
  retry_transfer: { label: '仅重做转存', hint: '产物已完整，只重跑转存' },
  rotate_source: { label: '换源重试', hint: '切换到下一个备选源' },
  discard_partial: { label: '清残留重下', hint: '删除产物与断点，从零重下' },
  retry_transfer_overwrite: { label: '覆盖转存', hint: '目标已存在时覆盖写入（显式 opt-in）' },
  retry_from_url: { label: '换链接重试', hint: '提供新的源 URL（保留断点）' },
};

// ctActionsFor 返回任务可用动作（优先服务端下发的 actions；否则按本地判据兜底）。
function ctActionsFor(t) {
  if (!t) return [];
  if (Array.isArray(t.actions) && t.actions.length) return t.actions;
  if (t.status !== 'failed' && t.status !== 'cancelled') return [];
  const plan = [{ action: 'retry_download', label: CT_ACTIONS.retry_download.label, hint: CT_ACTIONS.retry_download.hint }];
  const transfer = { action: 'retry_transfer', label: CT_ACTIONS.retry_transfer.label, hint: CT_ACTIONS.retry_transfer.hint };
  if (!t.download_done) { transfer.disabled = true; transfer.reason = '下载未完成'; }
  else if (!t.transfer) { transfer.disabled = true; transfer.reason = '无转存目标'; }
  plan.push(transfer);
  const rotate = { action: 'rotate_source', label: CT_ACTIONS.rotate_source.label, hint: CT_ACTIONS.rotate_source.hint };
  if (!Array.isArray(t.fallback_sources) || t.fallback_sources.length < 2) {
    rotate.disabled = true; rotate.reason = '未提供备选源';
  }
  const overwrite = { action: 'retry_transfer_overwrite', label: CT_ACTIONS.retry_transfer_overwrite.label,
    hint: CT_ACTIONS.retry_transfer_overwrite.hint };
  if (transfer.disabled) { overwrite.disabled = true; overwrite.reason = transfer.reason; }
  plan.push(overwrite);
  plan.push({ action: 'retry_from_url', label: CT_ACTIONS.retry_from_url.label,
    hint: CT_ACTIONS.retry_from_url.hint, needs_url: true });
  plan.push(rotate);
  plan.push({ action: 'discard_partial', label: CT_ACTIONS.discard_partial.label, hint: CT_ACTIONS.discard_partial.hint });
  return plan;
}

// ctActionControlsHtml 渲染「动作下拉 + 执行」控件（仅失败/取消行；无可用动作返回空串）。
function ctActionControlsHtml(t) {
  const plan = ctActionsFor(t);
  if (!plan.length) return '';
  let opts = '';
  plan.forEach(function (p) {
    const dis = p.disabled ? ' disabled' : '';
    const reason = p.disabled && p.reason ? '（' + p.reason + '）' : '';
    opts += '<option value="' + ctEsc(p.action) + '"' + dis + '>' + ctEsc(p.label + reason) + '</option>';
  });
  const firstEnabled = plan.filter(function (p) { return !p.disabled; }).length > 0;
  return '<span class="ct-action-wrap" style="margin-left:6px;white-space:nowrap;">' +
    '<select class="ct-action-select" data-id="' + ctEsc(t.id) + '" style="font-size:12px;">' + opts + '</select>' +
    '<button class="ct-action-apply" data-id="' + ctEsc(t.id) + '" style="margin-left:4px;"' +
    (firstEnabled ? '' : ' disabled') + '>执行</button></span>';
}

// ctActionDisabled 判断某动作在当前计划里是否禁用（未知动作按禁用处理：不猜可用）。
function ctActionDisabled(plan, action) {
  const list = Array.isArray(plan) ? plan : [];
  for (let i = 0; i < list.length; i++) {
    if (list[i] && list[i].action === action) return !!list[i].disabled;
  }
  return true;
}

// ctBatchActionOptionsHtml 渲染批量动作下拉（只列可批量化的动作；换链接需逐个输入，故不含）。
function ctBatchActionOptionsHtml() {
  const order = ['retry_download', 'retry_transfer', 'retry_transfer_overwrite', 'rotate_source', 'discard_partial'];
  return order.map(function (a) {
    const info = CT_ACTIONS[a] || { label: a };
    return '<option value="' + ctEsc(a) + '">' + ctEsc(info.label) + '</option>';
  }).join('');
}

// ctActionNeedsURL 判断动作是否需要用户提供额外参数（目前仅 retry_from_url）。
function ctActionNeedsURL(plan, action) {
  const list = Array.isArray(plan) ? plan : [];
  for (let i = 0; i < list.length; i++) {
    if (list[i] && list[i].action === action) return !!list[i].needs_url;
  }
  return false;
}

// ctBlockMatrix 把分块状态转成「一行多小块」的矩阵数据（用户 2026-11-26）。
// 每块按 1MiB 拆小块（上限 64 小块/行，避免一行过宽）；小块状态：done/active/todo。
function ctBlockMatrix(chunks) {
  const list = (Array.isArray(chunks) ? chunks : []).slice().sort(function (a, b) {
    return Number(a && a.index || 0) - Number(b && b.index || 0);
  });
  const SUB = 1048576;
  return list.map(function (c) {
    const len = Number(c && c.length || 0);
    const done = Math.max(0, Math.min(Number(c && c.done || 0), len));
    const total = Math.max(1, Math.min(64, Math.ceil(len / SUB) || 1));
    const filled = Math.min(total, Math.floor(done / SUB));
    const blocks = [];
    for (let i = 0; i < total; i++) {
      if (i < filled) blocks.push('done');
      else if (i === filled && done > 0 && done < len) blocks.push('active');
      else blocks.push(c && c.phase === 'failed' ? 'failed' : 'todo');
    }
    return {
      index: Number(c && c.index || 0),
      phase: (c && c.phase) || 'todo',
      source: (c && c.source) || '',
      done: done,
      length: len,
      blocks: blocks,
    };
  });
}

// ctBlockMatrixHtml 渲染块矩阵（无分块状态 ⇒ 明确占位，不伪造）。
function ctBlockMatrixHtml(chunks) {
  const rows = ctBlockMatrix(chunks);
  if (!rows.length) {
    return '<span style="color:var(--text-muted);">暂无分块状态（下载开始后按 1MiB 小块显示）</span>';
  }
  const color = { done: '#2da44e', active: '#1f6feb', todo: 'rgba(127,127,127,.25)', failed: '#c0392b' };
  let html = '<div class="ct-block-matrix" style="font-family:monospace;font-size:10px;line-height:1;">';
  rows.forEach(function (r) {
    html += '<div class="ct-block-row" data-chunk="' + r.index + '" style="white-space:nowrap;">';
    r.blocks.forEach(function (b) {
      html += '<span class="ct-block ct-block-' + b + '" style="display:inline-block;width:7px;height:10px;margin-right:1px;background:' +
        color[b] + ';"></span>';
    });
    html += '<span style="margin-left:6px;color:var(--text-muted);">#' + r.index + ' ' + r.phase +
      (r.source ? ' · ' + r.source : '') + '</span></div>';
  });
  return html + '</div>';
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
    ['速率曲线', '<span class="ct-spark-slot" data-id="' + ctEsc(t.id) + '">加载中…</span>'],
    ['分块进度', '<span class="ct-matrix-slot" data-id="' + ctEsc(t.id) + '">加载中…</span>'],
  ];
  let html = '<tr class="ct-detail" data-detail-for="' + ctEsc(t.id) + '"><td colspan="7" ' +
    'style="background:rgba(127,127,127,.06);padding:10px 12px;"><table style="font-size:12px;">';
  rows.forEach(function (r) {
    const isHtml = r[0] === '速率曲线' || r[0] === '分块进度'; // 这两行由异步填充（SVG/矩阵）
    html += '<tr><td style="color:var(--text-muted);padding:2px 12px 2px 0;white-space:nowrap;">' + ctEsc(r[0]) +
      '</td><td style="padding:2px 0;word-break:break-all;">' + (isHtml ? r[1] : ctEsc(r[1])) + '</td></tr>';
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
    // 可点击 chip：一键批量重试该类别（源类失败服务端会自动换源）。
    return '<button class="ct-class-chip" data-class="' + ctEsc(k) +
      '" style="border:none;background:rgba(192,57,43,.12);color:#c0392b;border-radius:10px;' +
      'padding:2px 8px;margin-right:4px;cursor:pointer;font-size:12px;" title="批量重试该类别">' +
      ctEsc(ctClassInfo(k).label) + ' ' + cls[k] + '</button>';
  });
  let html = '<div style="font-size:13px;margin-bottom:6px;">总 ' + s.total +
    '｜进行中 ' + s.running + '｜完成 ' + s.completed + '｜失败 ' + s.failed +
    '｜取消 ' + s.cancelled;
  if (failParts.length) html += '（' + failParts.join('') + '）';
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
      ctActionControlsHtml(t) +
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
    '<select id="ct-batch-action" style="margin-left:10px;font-size:12px;">' + ctBatchActionOptionsHtml() + '</select>' +
    '<button id="ct-batch-apply" style="margin-left:4px;">批量执行动作</button>' +
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
    ctGroupByVolume: ctGroupByVolume,
    ctGroupByVolumeHtml: ctGroupByVolumeHtml,
    ctPageRange: ctPageRange,
    ctSortTasks: ctSortTasks,
    ctAppendAudit: ctAppendAudit,
    ctAuditHtml: ctAuditHtml,
    CT_AUDIT_KEY: CT_AUDIT_KEY,
    CT_AUDIT_CAP: CT_AUDIT_CAP,
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
    ctClassRetryPayloads: ctClassRetryPayloads,
    ctDetailHtml: ctDetailHtml,
    ctRateSeries: ctRateSeries,
    ctSparklinePath: ctSparklinePath,
    ctSparklineSvg: ctSparklineSvg,
    ctRateText: ctRateText,
    ctActionControlsHtml: ctActionControlsHtml,
    ctActionsFor: ctActionsFor,
    ctActionDisabled: ctActionDisabled,
    ctActionNeedsURL: ctActionNeedsURL,
    ctBatchActionOptionsHtml: ctBatchActionOptionsHtml,
    CT_ACTIONS: CT_ACTIONS,
    ctBlockMatrix: ctBlockMatrix,
    ctBlockMatrixHtml: ctBlockMatrixHtml,
    ctSummaryHtml: ctSummaryHtml,
    ctRowHtml: ctRowHtml,
    ctTableHtml: ctTableHtml,
    ctPanelHtml: ctPanelHtml,
  };
}

// ---- 浏览器侧交互（仅浏览器执行；node --test 下无 document，自动跳过） ----
if (typeof document !== 'undefined' && typeof window !== 'undefined') {
  // ctLoadAudit/ctSaveAudit 从 sessionStorage 读写操作记录（浏览器不可用/存储异常时静默）。
  function ctLoadAudit() {
    try {
      const raw = sessionStorage.getItem(CT_AUDIT_KEY);
      const parsed = raw ? JSON.parse(raw) : [];
      return Array.isArray(parsed) ? parsed : [];
    } catch (e) { return []; }
  }
  function ctSaveAudit(log) {
    try { sessionStorage.setItem(CT_AUDIT_KEY, JSON.stringify(log)); } catch (e) { /* 配额/隐私模式忽略 */ }
  }
  // ctRecord 记录一条操作（供重试/取消/导入/批量共用）。
  function ctRecord(action, target, result, detail) {
    const log = ctAppendAudit(ctLoadAudit(), { action: action, target: target, result: result, detail: detail });
    ctSaveAudit(log);
    return log;
  }
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

  // ctFillSparkline 异步拉取任务进度采样并填充展开行的速率曲线（用户 2026-11-26）。
  // 失败不打断面板（只在该槽位给出原因），避免"看不到曲线"演变成"整页报错"。
  async function ctFillSparkline(panel, id) {
    const slot = panel.querySelector('.ct-spark-slot[data-id="' + id + '"]');
    if (!slot) return;
    try {
      const res = await window.sclientTransport.coreRequest(
        'GET', '/api/cloud/tasks/' + encodeURIComponent(id) + '/progress', {});
      const data = window.sclientUtil.decodeJSON(res.body);
      const svg = ctSparklineSvg((data && data.samples) || [], 220, 40);
      slot.innerHTML = svg || '<span style="color:var(--text-muted);">暂无采样（任务尚未产生进度）</span>';
    } catch (e) {
      slot.innerHTML = '<span style="color:#c0392b;">进度加载失败：' + ctEsc(e.message) + '</span>';
    }
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
      minSizeMB: st.minSizeMB || 0,
      sort: st.sort || 'updated',
      sortDir: st.sortDir || 'desc',
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
      '<label>≥ <input id="ct-minsize" type="number" min="0" step="100" placeholder="MiB" value="' +
      (filters.minSizeMB ? filters.minSizeMB : '') + '" style="width:80px;"> MiB</label>' +
      '<label>排序 <select id="ct-sort">' +
      [['updated', '更新时间'], ['status', '状态'], ['name', '文件名'], ['size', '体积']]
        .map(function (o) {
          return '<option value="' + o[0] + '"' + (filters.sort === o[0] ? ' selected' : '') + '>' + o[1] + '</option>';
        }).join('') + '</select></label>' +
      '<label><select id="ct-sort-dir">' +
      [['desc', '降序'], ['asc', '升序']].map(function (o) {
        return '<option value="' + o[0] + '"' + (filters.sortDir === o[0] ? ' selected' : '') + '>' + o[1] + '</option>';
      }).join('') + '</select></label>' +
      '<button id="ct-export">导出TSV</button>' +
      '<label style="cursor:pointer;">导入TSV重试<input id="ct-import" type="file" accept=".tsv,text/tab-separated-values" style="display:none;"></label>' +
      '</div>' + (function renderPaged() {
        // 分页在**过滤之后**做（过滤是客户端全量语义，服务端未提供卷/类别过滤）。
        const filtered = ctSortTasks(ctFilterTasks(tasks, filters), filters.sort, filters.sortDir);
        const limit = 100;
        const [start, end] = ctPageRange(window.__ctOffset || 0, limit, filtered.length);
        window.__ctOffset = start;
        return ctSummaryHtml(tasks) + ctGroupByVolumeHtml(tasks) + ctTableHtml(filtered.slice(start, end)) +
          ctPagerHtml(filtered.length, start, limit) + ctAuditHtml(ctLoadAudit(), 8);
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
        ctRecord('导入重试', ok + ' 个', failed ? 'failed' : 'ok', '跳过 ' + parsed.skipped + ' 行');
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
    const minSizeEl = document.getElementById('ct-minsize');
    if (minSizeEl) minSizeEl.addEventListener('change', function () {
      st.minSizeMB = Number(minSizeEl.value) || 0; window.__ctOffset = 0; void window.showCloudTasks();
    });
    panel.querySelectorAll('.ct-vol-chip').forEach(function (chip) {
      chip.addEventListener('click', function () {
        st.volume = chip.getAttribute('data-volume');
        window.__ctOffset = 0;
        if (typeof window.syncCloudFiltersHash === 'function') window.syncCloudFiltersHash();
        void window.showCloudTasks();
      });
    });

    panel.querySelectorAll('.ct-class-chip').forEach(function (chip) {
      chip.addEventListener('click', async function () {
        const cls = chip.getAttribute('data-class');
        const payloads = ctClassRetryPayloads(tasks, cls);
        if (!payloads.length) { setCtMsg('该类别的任务均不可重试'); return; }
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
        ctRecord('按类别重试', ctClassInfo(cls).label, failed ? 'failed' : 'ok', ok + ' 个');
        setCtMsg('按类别重试（' + ctClassInfo(cls).label + '）：提交 ' + ok + ' 个，失败 ' + failed + ' 个');
        await window.showCloudTasks();
      });
    });

    const sortSel = document.getElementById('ct-sort');
    if (sortSel) sortSel.addEventListener('change', function () { st.sort = sortSel.value; void window.showCloudTasks(); });
    const sortDirSel = document.getElementById('ct-sort-dir');
    if (sortDirSel) sortDirSel.addEventListener('change', function () { st.sortDir = sortDirSel.value; void window.showCloudTasks(); });
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

    // 失败任务动作：下拉选动作 → 执行 → POST /api/cloud/tasks/{id}/actions → 刷新面板。
    panel.querySelectorAll('.ct-action-apply').forEach(function (btn) {
      btn.addEventListener('click', async function () {
        const id = btn.getAttribute('data-id');
        const sel = panel.querySelector('.ct-action-select[data-id="' + id + '"]');
        const action = sel ? sel.value : '';
        if (!action) { setCtMsg('请选择动作'); return; }
        const t = (window.__ctLastTasks || []).filter(function (x) { return x && x.id === id; })[0];
        if (ctActionDisabled(ctActionsFor(t), action)) {
          setCtMsg('该动作当前不可用（' + (sel && sel.selectedOptions && sel.selectedOptions[0]
            ? sel.selectedOptions[0].textContent : action) + '）');
          return;
        }
        const payload = { action: action };
        if (ctActionNeedsURL(ctActionsFor(t), action)) {
          const input = window.prompt('请输入新的源 URL（retry_from_url）：', '');
          if (!input) { setCtMsg('已取消（未提供 URL）'); return; }
          payload.url = input.trim();
        }
        btn.disabled = true;
        try {
          await window.sclientTransport.coreRequest('POST',
            '/api/cloud/tasks/' + encodeURIComponent(id) + '/actions',
            { bodyBytes: new TextEncoder().encode(JSON.stringify(payload)) });
          ctRecord('动作:' + action, id, 'ok', '');
          setCtMsg('已提交动作：' + action);
          await window.showCloudTasks();
        } catch (e) {
          ctRecord('动作:' + action, id, 'failed', e.message);
          setCtMsg('动作失败：' + e.message);
          btn.disabled = false;
        }
      });
    });

    // 批量动作：对勾选的失败任务串行执行同一动作（服务端逐项结果）。
    const batchActionBtn = document.getElementById('ct-batch-apply');
    if (batchActionBtn) batchActionBtn.addEventListener('click', async function () {
      const ids = Array.prototype.slice.call(panel.querySelectorAll('.ct-pick:checked'))
        .map(function (cb) { return cb.getAttribute('data-id'); });
      if (!ids.length) { setCtMsg('请先勾选失败任务'); return; }
      const sel = document.getElementById('ct-batch-action');
      const action = sel ? sel.value : '';
      if (!action) { setCtMsg('请选择批量动作'); return; }
      batchActionBtn.disabled = true;
      try {
        const res = await window.sclientTransport.coreRequest('POST', '/api/cloud/tasks/actions/batch',
          { bodyBytes: new TextEncoder().encode(JSON.stringify({ action: action, ids: ids })) });
        const data = window.sclientUtil.decodeJSON(res.body) || {};
        ctRecord('批量动作:' + action, ids.join(','), data.failed ? 'partial' : 'ok',
          '成功 ' + (data.succeeded || 0) + '，失败 ' + (data.failed || 0));
        setCtMsg('批量动作 ' + action + '：成功 ' + (data.succeeded || 0) + '，失败 ' + (data.failed || 0));
        await window.showCloudTasks();
      } catch (e) {
        ctRecord('批量动作:' + action, ids.join(','), 'failed', e.message);
        setCtMsg('批量动作失败：' + e.message);
        batchActionBtn.disabled = false;
      }
    });

    // 下拉切换时同步「执行」按钮可用性（禁用动作不得可点）。
    panel.querySelectorAll('.ct-action-select').forEach(function (sel) {
      sel.addEventListener('change', function () {
        const id = sel.getAttribute('data-id');
        const t = (window.__ctLastTasks || []).filter(function (x) { return x && x.id === id; })[0];
        const btn = panel.querySelector('.ct-action-apply[data-id="' + id + '"]');
        if (btn) btn.disabled = ctActionDisabled(ctActionsFor(t), sel.value);
      });
    });

    // 详情展开/收起（行内插入 ct-detail 行；再次点击移除）。
    panel.querySelectorAll('.ct-detail-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        const id = btn.getAttribute('data-id');
        const exists = panel.querySelector('.ct-detail[data-detail-for="' + id + '"]');
        if (exists) { exists.remove(); return; }
        const row = btn.closest('tr');
        const t = (window.__ctLastTasks || []).filter(function (x) { return x && x.id === id; })[0];
        if (row && t) {
          row.insertAdjacentHTML('afterend', ctDetailHtml(t));
          const slot = panel.querySelector('.ct-matrix-slot[data-id="' + id + '"]');
          if (slot) slot.innerHTML = ctBlockMatrixHtml(t.chunks);
          void ctFillSparkline(panel, id);
        }
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
      ctRecord('批量取消', ok + ' 个', failed ? 'failed' : 'ok', '失败 ' + failed);
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
        ctRecord('批量重试', ok + ' 个', failed ? 'failed' : 'ok', '失败 ' + failed);
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
          ctRecord('重试', id, 'ok');
          await window.showCloudTasks();
        } catch (e) {
          ctRecord('重试', id, 'failed', e.message);
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
          ctRecord('取消', id, 'ok');
          await window.showCloudTasks();
        } catch (e) {
          ctRecord('取消', id, 'failed', e.message);
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
