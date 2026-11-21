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
  return list.filter(function (t) {
    if (!t) return false;
    if (f.status && t.status !== f.status) return false;
    if (f.failedOnly && t.status !== 'failed' && t.status !== 'cancelled') return false;
    return true;
  });
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
  return '<tr data-id="' + ctEsc(t.id) + '">' +
    '<td style="color:' + ctStatusColor(t.status) + ';">' + ctEsc(ctStatusLabel(t.status)) + '</td>' +
    '<td>' + ctEsc(t.filename || t.url || '') + '</td>' +
    '<td>' + (pct === null ? '-' : pct + '%') + '</td>' +
    '<td>' + ctEsc(ctStageLabel(d.stage)) + '</td>' +
    '<td>' + failure + '</td>' +
    '<td>' + ctEsc(d.partial_size ? (Math.round(d.partial_size / 1048576) + ' MiB') : '-') + '</td>' +
    '<td>' + retryBtn + cancelBtn + '</td>' +
    '</tr>';
}

// ctTableHtml 整表渲染（含表头；空列表提示）。
function ctTableHtml(tasks) {
  const list = Array.isArray(tasks) ? tasks : [];
  if (list.length === 0) return '';
  let html = '<table style="width:100%;font-size:12px;border-collapse:collapse;"><thead><tr>' +
    '<th>状态</th><th>文件</th><th>进度</th><th>阶段</th><th>失败原因</th><th>已下载</th><th>操作</th>' +
    '</tr></thead><tbody>';
  list.forEach(function (t) { html += ctRowHtml(t); });
  return html + '</tbody></table>';
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
    ctSummarize: ctSummarize,
    ctGroupByClass: ctGroupByClass,
    ctRetryPayload: ctRetryPayload,
    ctSummaryHtml: ctSummaryHtml,
    ctRowHtml: ctRowHtml,
    ctTableHtml: ctTableHtml,
    ctPanelHtml: ctPanelHtml,
  };
}

// ---- 浏览器侧交互（仅浏览器执行；node --test 下无 document，自动跳过） ----
if (typeof document !== 'undefined' && typeof window !== 'undefined') {
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
    const filters = { failedOnly: !!(failedOnly && failedOnly.checked) };
    let tasks = [];
    try {
      tasks = await ctFetchTasks();
    } catch (e) {
      panel.innerHTML = '<div style="color:#c0392b;font-size:13px;">加载失败：' + ctEsc(e.message) + '</div>';
      return;
    }
    panel.innerHTML =
      '<div style="display:flex;gap:12px;align-items:center;margin-bottom:8px;font-size:13px;">' +
      '<label><input type="checkbox" id="ct-failed-only"' + (filters.failedOnly ? ' checked' : '') + '> 只看失败/取消</label>' +
      '<label><input type="checkbox" id="ct-auto"> 自动刷新(5s)</label>' +
      '<button id="ct-refresh">刷新</button>' +
      '</div>' + ctPanelHtml(tasks, filters);

    // 事件绑定（每次重渲染后重绑；控件是面板内新建的）。
    const refreshBtn = document.getElementById('ct-refresh');
    if (refreshBtn) refreshBtn.addEventListener('click', function () { void window.showCloudTasks(); });
    const fOnly = document.getElementById('ct-failed-only');
    if (fOnly) fOnly.addEventListener('change', function () { void window.showCloudTasks(); });

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
