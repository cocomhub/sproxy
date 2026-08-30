/* SPDX-License-Identifier: Apache-2.0 */
/* global module */
/*
 * app-render.js —— app 层纯渲染模块（与 DOM/网络完全隔离，可 node:test 直测）。
 *
 * 隔离原则（全部 app 层共同遵守）：
 *   1. 纯函数生成 HTML 字符串 / 格式化 / 映射，不碰 DOM、不读模块状态、不碰全局；
 *   2. app.js 只做 DOM 写入（innerHTML / textContent）与网络/流程，凡此处有的函数
 *      一律走本模块（appRender.*），不再内联重写；
 *   3. 本文件 UMD：浏览器挂全局 appRender，Node module.exports 可 require。
 *      顶层无任何副作用（不引用 document/window/localStorage）。
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.appRender = factory();
  }
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // ---- 基础工具（纯） ----

  function escHtml(s) {
    return String(s == null ? '' : s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
  }

  // 统一大小格式化：既有 formatSize 与 formatBytes 两份不同实现，合并为一个（GB 档）。
  // <1KB → "N B"；<1MB → KB；<1GB → MB；否则 GB；null/undefined → "-"。
  function formatSize(n) {
    if (n == null) return '-';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(2) + ' GB';
  }

  // stripCloudId：把云项展示 id（'cloud-task-<id>' / 'cloud-group-<id>'）还原为服务端真实 id。
  // 云行按钮 data-id 携带展示 id（含前缀）；凡要调 sc.cloud.* API 或拼 '.__cloud__/<id>/' 路径，
  // 必须先剥前缀，否则后端 404/组 not found。非云 id 原样返回。双实现同源同行为：
  // app.js（浏览器全局，脚本顺序在其后加载并定义覆盖本镜像）用于浏览器运行时；
  // 本镜像服务 node 测试环境（无 app.js），测试直接验证本体。
  function stripCloudId(id) {
    return String(id).replace(/^cloud-(task|group)-/, '');
  }

  function getChecksumPrefix(cs) {
    if (!cs) return '-';
    return cs.substring(0, 16) + '…';
  }

  function bytesToHex(bytes) {
    if (!bytes) return '';
    let out = '';
    for (let i = 0; i < bytes.length; i++) out += bytes[i].toString(16).padStart(2, '0');
    return out;
  }

  // 文件列表响应归一：服务端可能是数组或 {files:[...]}（多处重复），统一取数组。
  function normalizeList(data, key) {
    const k = key || 'files';
    if (Array.isArray(data)) return data;
    if (data && Array.isArray(data[k])) return data[k];
    return [];
  }

  // zipNames(urls, filenames) → [{url, filename}]；filenames 可能短于 urls（缺省用 ''）。
  function zipNames(urls, filenames) {
    return (urls || []).map(function (url, idx) {
      return { url: url, filename: (filenames && filenames[idx]) || '' };
    });
  }

  // parseCloudLines(text)：多行文本 → [{url, preset}]（Tab 分割，多余 Tab 忽略）。
  // 空行过滤；单行无 Tab → {url, preset:''}。纯函数。
  function parseCloudLines(text) {
    if (typeof text !== 'string' || !text.trim()) return [];
    const out = [];
    for (const line of text.split('\n')) {
      const t = line.trim();
      if (!t) continue;
      const parts = t.split('\t');
      out.push({ url: parts[0].trim(), preset: parts.length > 1 ? parts[1].trim() : '' });
    }
    return out;
  }

  // previewKind(name)：扩展名 → 'image' | 'text' | 'download'（previewFile 的归类逻辑）。
  const IMAGE_EXT = ['jpg', 'jpeg', 'png', 'gif', 'webp', 'svg', 'bmp', 'ico'];
  const TEXT_EXT = ['txt', 'md', 'json', 'yaml', 'yml', 'xml', 'csv', 'log', 'sh', 'bat', 'go', 'js', 'py', 'css', 'html', 'conf', 'ini', 'cfg'];
  function previewKind(name) {
    if (typeof name !== 'string' || !name) return 'download';
    const dot = name.lastIndexOf('.');
    if (dot < 0) return 'download';
    const ext = name.slice(dot + 1).toLowerCase();
    if (IMAGE_EXT.indexOf(ext) >= 0) return 'image';
    if (TEXT_EXT.indexOf(ext) >= 0) return 'text';
    return 'download';
  }

  // ---- 文件列表渲染（纯 HTML 字符串） ----
  function buildFileTableHtml(files, subdir) {
    let html = '<table id="file-table"><thead><tr><th class="check-col"><input type="checkbox" id="select-all-checkbox"></th><th>文件名</th><th>大小</th><th>Checksum (SHA-256)</th><th>操作</th></tr></thead><tbody>';
    for (const fi of files || []) {
      const fullName = subdir ? subdir + '/' + fi.name : fi.name;
      html += buildFileRowHtml(fi, fullName);
    }
    html += '</tbody></table>';
    return html;
  }

  function buildFileRowHtml(fi, fullName) {
    if (fi.is_dir) {
      return '<tr style="cursor:pointer;background:var(--bg-dir);" class="dir-row" data-subdir="' + escHtml(fullName) + '"><td class="check-col"></td><td><strong>' + escHtml(fi.name) + '/</strong></td>' +
        '<td>-</td><td>-</td><td>' +
        '<button class="btn btn-sm btn-secondary dir-enter-btn" data-subdir="' + escHtml(fullName) + '">进入</button>' +
        '<button class="btn btn-sm btn-primary dir-archive-btn" data-subdir="' + escHtml(fullName) + '">打包下载</button>' +
        '<button class="btn btn-sm btn-danger dir-delete-btn" data-subdir="' + escHtml(fullName) + '">删除</button></td></tr>';
    }
    const cs = fi.checksum || '';
    const csDisplay = cs ? '<span class="checksum-cell" data-checksum="' + escHtml(cs) + '" title="' + escHtml(cs) + '">' + escHtml(getChecksumPrefix(cs)) + '<span class="copy-icon">📋</span></span>' : '-';
    return '<tr><td class="check-col"><input type="checkbox" class="file-select" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '"></td><td class="overflow-dots" title="' + escHtml(fullName) + '">' + escHtml(fi.name) + '</td>' +
      '<td class="size-cell">' + formatSize(fi.size) + '</td>' +
      '<td>' + csDisplay + '</td>' +
      '<td class="file-actions">' +
      '<button class="btn btn-primary btn-sm file-download-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">下载</button>' +
      '<button class="btn btn-sm btn-secondary file-preview-btn" data-filename="' + escHtml(fullName) + '">预览</button>' +
      '<button class="btn btn-danger btn-sm file-delete-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">删除</button>' +
      '<button class="btn btn-warning btn-sm file-rename-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">重命名</button>' +
      '<button class="btn btn-sm btn-share file-share-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">分享</button>' +
      '</td></tr>';
  }

  // 参数化版（不再读模块状态 _hasMore/_currentOffset——两个量由调用方传入）。
  function buildLoadMoreHtml(total, offset, hasMore) {
    if (!hasMore) return '';
    const remaining = Math.max(0, (total || 0) - (offset || 0));
    return '<div id="load-more-container" style="text-align:center;padding:12px;">' +
      '<button class="btn btn-primary">加载更多 (' + remaining + ')</button></div>';
  }

  // 已加载全部 的尾行文案（loadMore 收尾复用）。
  function buildAllLoadedHtml(total) {
    return '<div style="text-align:center;padding:12px;color:var(--text-muted);">已加载全部 ' + (total || 0) + ' 个文件</div>';
  }

  // ---- hub / config / stats ----
  function hubTableHtml(nodes, stats) {
    var html = '';
    if (stats) {
      html += '<div style="margin-bottom:12px;padding:8px 12px;background:var(--stats-summary);border-radius:4px;font-size:13px;">';
      html += '已连接节点: <strong>' + (stats.nodes_connected ?? 0) + '</strong></div>';
    }
    if (!nodes || nodes.length === 0) {
      html += '<div class="empty-msg">暂无已连接节点</div>';
      return html;
    }
    html += '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:var(--bg-hover);">';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">节点 ID</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">地址</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">连接时间</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid var(--border-color);">操作</th>';
    html += '</tr></thead><tbody>';
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      var connected = n.connected ? new Date(n.connected).toLocaleString() : '-';
      html += '<tr>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-family:monospace;font-size:12px;">' + escHtml(n.id) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + escHtml(n.addr || '-') + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;">' + connected + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">';
      html += '<button class="btn btn-danger btn-sm hub-remove-btn" data-node-id="' + escHtml(n.id) + '">移除</button>';
      html += '</td></tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  function configTableHtml(cfg) {
    var html = '<table style="width:100%;border-collapse:collapse;font-size:14px;">';
    html += '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary)">运行时配置</th></tr>';
    function row(label, value) {
      return '<tr><td style="padding:5px 0;color:var(--text-secondary)">' + label + '</td><td style="text-align:right">' + (value ?? '-') + '</td></tr>';
    }
    html += row('日志级别', cfg.log_level);
    html += row('日志格式', cfg.log_format);
    html += row('AccessKey 认证', cfg.access_keys_set ? '✅ 已设置' : '❌ 未设置');
    html += row('速率限制', cfg.rate_limit_requests + ' req / ' + (cfg.rate_limit_window || '-'));
    html += row('存储上限', cfg.max_storage_bytes > 0 ? formatSize(cfg.max_storage_bytes) : '不限');
    html += row('分块大小', formatSize(cfg.chunk_size));
    html += row('上传会话 TTL', cfg.upload_session_ttl || '-');
    html += row('版本管理', cfg.versioning_enabled ? '✅ 启用' : '❌ 关闭');
    html += row('云端并发', cfg.cloud_max_concurrent);
    html += row('地址', cfg.addr);
    html += row('上传目录', cfg.uploads_dir);
    html += row('TLS', cfg.tls_enabled ? '✅ 启用' : '❌ 关闭');
    html += row('Hub 中继', cfg.hub_enabled ? '✅ 启用' : '❌ 关闭');
    html += '</table>';

    // 配置编辑区
    html += '<div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--border-color);">';
    html += '<div style="font-size:13px;font-weight:600;color:var(--text-secondary);margin-bottom:8px;">快速编辑</div>';
    html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
    html += '<span style="font-size:13px;color:var(--text-secondary);">日志级别:</span>';
    html += '<select id="cfg-log-level" style="padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
    var levels = ['debug', 'info', 'warn', 'error'];
    for (var i = 0; i < levels.length; i++) {
      html += '<option value="' + levels[i] + '"' + (cfg.log_level === levels[i] ? ' selected' : '') + '>' + levels[i] + '</option>';
    }
    html += '</select>';
    html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-level">更新</button></div>';
    html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
    html += '<span style="font-size:13px;color:var(--text-secondary);">日志格式:</span>';
    html += '<select id="cfg-log-format" style="padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
    html += '<option value="text"' + (cfg.log_format === 'text' ? ' selected' : '') + '>text</option>';
    html += '<option value="json"' + (cfg.log_format === 'json' ? ' selected' : '') + '>json</option>';
    html += '</select>';
    html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-format">更新</button></div>';
    html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
    html += '<span style="font-size:13px;color:var(--text-secondary);">速率限制:</span>';
    html += '<input type="number" id="cfg-rate-limit" value="' + (cfg.rate_limit_requests ?? 10) + '" style="width:60px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
    html += '<span style="font-size:12px;color:var(--text-muted);">req / </span>';
    html += '<input type="text" id="cfg-rate-window" value="' + (cfg.rate_limit_window || '1s') + '" style="width:60px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
    html += '<button class="btn btn-sm btn-primary" id="cfg-update-rate-limit">更新</button></div>';
    html += '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;">';
    html += '<span style="font-size:13px;color:var(--text-secondary);">存储上限:</span>';
    html += '<input type="number" id="cfg-max-storage" value="' + (cfg.max_storage_bytes ?? 0) + '" style="width:140px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;" min="0">';
    html += '<span style="font-size:12px;color:var(--text-muted);">字节（0=不限）</span>';
    html += '<button class="btn btn-sm btn-primary" id="cfg-update-storage">更新</button></div>';
    html += '</div>';
    return html;
  }

  function statsTableHtml(du, rc, s) {
    return '<table style="width:100%;border-collapse:collapse;font-size:14px;">' +
      '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary)">磁盘使用</th></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">目录</td><td style="text-align:right">' + ((du && du.uploads_dir) || '-') + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">文件数</td><td style="text-align:right">' + ((du && du.total_files) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">总大小</td><td style="text-align:right">' + formatSize(du && du.total_size) + '</td></tr>' +
      '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary);padding-top:14px">请求统计（自启动）</th></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">总请求数</td><td style="text-align:right">' + ((rc && rc.total) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">2xx</td><td style="text-align:right">' + ((rc && rc['2xx']) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">4xx</td><td style="text-align:right">' + ((rc && rc['4xx']) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">5xx</td><td style="text-align:right">' + ((rc && rc['5xx']) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">活跃连接</td><td style="text-align:right">' + ((s && s.active_connections) ?? 0) + '</td></tr>' +
      '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary);padding-top:14px">传输统计（自启动）</th></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">上传文件数</td><td style="text-align:right">' + ((s && s.files_uploaded) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">上传字节数</td><td style="text-align:right">' + formatSize(s && s.bytes_uploaded) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">下载文件数</td><td style="text-align:right">' + ((s && s.files_downloaded) ?? 0) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">下载字节数</td><td style="text-align:right">' + formatSize(s && s.bytes_downloaded) + '</td></tr>' +
      '<tr><td style="padding:5px 0;color:var(--text-secondary)">删除文件数</td><td style="text-align:right">' + ((s && s.files_deleted) ?? 0) + '</td></tr></table>';
  }

  // ---- 云端任务 / 组 ----
  function statusText(status) {
    switch (status) {
      case 'pending': return '⏳ 等待中';
      case 'downloading': return '⬇ 下载中';
      case 'completed': return '✅ 已完成';
      case 'failed': return '❌ 失败';
      case 'cancelled': return '🚫 已取消';
      default: return status;
    }
  }

  function buildProgressBar(downloaded, total) {
    const pct = total > 0 ? Math.min(100, Math.round(downloaded * 100 / total)) : 0;
    return '<div style="margin-top:4px;height:6px;background:var(--progress-bg);border-radius:3px;overflow:hidden;min-width:80px;">' +
      '<div style="height:100%;width:' + pct + '%;background:var(--progress-bar);border-radius:3px;transition:width 0.5s;"></div>' +
      '</div><div style="font-size:11px;color:var(--text-secondary);margin-top:1px;">' + formatSize(downloaded) + ' / ' + formatSize(total) + ' (' + pct + '%)</div>';
  }

  // 同步任务状态徽章文案（sync_task 频道专用）：复用 statusText 的 pending/completed/failed/
  // cancelled 语义，新增 syncing→🔄 同步中。未知/空回落原值。
  function syncStatusText(status) {
    switch (status) {
      case 'pending': return '⏳ 等待中';
      case 'syncing': return '🔄 同步中';
      case 'completed': return '✅ 已完成';
      case 'failed': return '❌ 失败';
      case 'cancelled': return '🚫 已取消';
      default: return status;
    }
  }

  // buildSyncRowMeta：从 sync_task TransferItem 提取展示元信息（纯函数，供单测与渲染）。
  // 字段来源：item 顶层（filename/src/dst/direction/loaded/total）优先，meta.sync 快照兜底。
  // 进度优先级：字节进度（bytes_done/bytes_total → item.loaded/total）> 文件进度（files_done/files_total）。
  function buildSyncRowMeta(item) {
    const meta = (item && item.meta && item.meta.sync) || {};
    const src = (item && item.src) || meta.src || '';
    const dst = (item && item.dst) || meta.dst || '';
    const direction = (item && item.direction) || meta.direction || '';
    const loaded = Number(item && item.loaded) > 0 ? Number(item && item.loaded) : 0;
    const total = Number(item && item.total) > 0 ? Number(item && item.total) : 0;
    const filesDone = Number(meta.files_done) > 0 ? Number(meta.files_done) : 0;
    const filesTotal = Number(meta.files_total) > 0 ? Number(meta.files_total) : 0;
    const bytesPct = total > 0 ? Math.min(100, Math.round(loaded * 100 / total)) : 0;
    const filesPct = filesTotal > 0 ? Math.min(100, Math.round(filesDone * 100 / filesTotal)) : 0;
    const hasBytes = total > 0;
    const hasFiles = filesTotal > 0;
    let progressText = '';
    if (hasBytes) {
      progressText = formatSize(loaded) + ' / ' + formatSize(total) + ' (' + bytesPct + '%)';
    } else if (hasFiles) {
      // 文件进度格式对齐云组 completed/total（无空格斜杠）。
      progressText = filesDone + '/' + filesTotal + ' 个文件 (' + filesPct + '%)';
    }
    return {
      title: (item && item.filename) || src || '同步任务',
      direction: direction,
      src: src,
      dst: dst,
      loaded: loaded,
      total: total,
      filesDone: filesDone,
      filesTotal: filesTotal,
      bytesPct: bytesPct,
      filesPct: filesPct,
      hasBytes: hasBytes,
      hasFiles: hasFiles,
      progressText: progressText,
    };
  }

  // 诊断云行可见性（R1 起不直接调用：云行统一走 _rowActions kind 分派 → _cloudTaskActions/
  // _cloudGroupActions，data-id 携带展示 id）。保留导出避免破坏既有调用方与测试引用。
  function cloudTaskActions(id, filename, status, checksum) { return _cloudTaskActions({ id: id, filename: filename, status: status, checksum: checksum || '', meta: { raw: {} } }); }

  function buildCloudTaskTableHtml(tasks) {
    let html = '<div style="font-size:13px;color:var(--text-warning);margin-bottom:6px;">(legacy 表格视图，已由传输页统一渲染管取代)</div><table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">文件名</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">大小</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';
    for (const t of tasks || []) {
      const statusLabel = statusText(t.status);
      const progressHtml = t.status === 'downloading' && t.total_size > 0 ? buildProgressBar(t.downloaded, t.total_size) : '';
      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(t.filename || '') + '">' + escHtml(t.filename || '-') + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + statusLabel + progressHtml + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' + (t.total_size > 0 ? formatSize(t.total_size) : '-') + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' +
        _cloudTaskActions({ id: t.id, kind: 'cloud_task', filename: t.filename, status: t.status, checksum: t.checksum, meta: { raw: { checksum: t.checksum } } }) + '</td></tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  function cloudGroupActions(id, status) { return _cloudGroupActions({ id: id, kind: 'cloud_group', status: status }); }

  function buildCloudGroupTableHtml(groups) {
    let html = '<div style="font-size:13px;color:var(--text-warning);margin-bottom:6px;">(legacy 表格视图，已由传输页统一渲染管取代)</div><table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">名称</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">进度</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);"></th></tr></thead><tbody>';
    for (const g of groups || []) {
      const statusLabel = statusText(g.status);
      const progressText = (g.completed || 0) + '/' + (g.total_tasks || 0);
      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(g.name || g.id) + '">' + escHtml(g.name || g.id) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + statusLabel + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + progressText + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' +
        _cloudGroupActions({ id: g.id, kind: 'cloud_group', status: g.status }) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);"></td></tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  // ---- 版本 ----
  function buildVersionTableHtml(versions, filename) {
    var html = '<div style="margin-bottom:8px;font-size:13px;color:var(--text-secondary);">文件: <strong>' + escHtml(filename) + '</strong>，共 ' + (versions ? versions.length : 0) + ' 个版本</div>';
    html += '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">版本 ID</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">时间</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">大小</th>' +
      '<th style="text-align:right;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';
    for (var i = 0; i < (versions ? versions.length : 0); i++) {
      var v = versions[i];
      var versionTime = v.created_at || '-';
      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-family:monospace;font-size:12px;">' + escHtml(String(v.version_id || '-')) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' + escHtml(versionTime) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' + formatSize(v.size) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:right;white-space:nowrap;">' +
        '<button class="btn btn-primary btn-sm version-restore-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '" style="margin-right:4px;">恢复</button>' +
        '<button class="btn btn-danger btn-sm version-delete-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '">删除</button></td></tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  // 上传进度文案（与 upload.js progressText 同语义——upload.js 已在本文件之下本地实现，
  // 重复不是缺陷。唯一依赖 formatSize 的跨文件调用点：upload.js 顶部进度回调）。
  // 本函数仅供单元测试（可测入口）与 upload 逻辑复用，避免直接调用依赖 DOM 的整体模块。
  function uploadProgressText(label, loaded, total, totalChunks, chunkIndex) {
    const sized = label || '上传中…';
    const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
    const sizeTxt = formatSize(loaded) + '/' + formatSize(total);
    let text;
    if (totalChunks && totalChunks > 1) {
      const idx = (chunkIndex || chunkIndex === 0 ? chunkIndex : 0) + 1;
      text = sized + ' ' + pct + '%（' + sizeTxt + '，分块 ' + Math.min(totalChunks, idx) + '/' + totalChunks + '）';
    } else {
      text = sized + ' ' + pct + '%（' + sizeTxt + '）';
    }
    return { pct: pct, text: text };
  }

  // ---- 传输渲染（纯函数，只读 TransferItem 数组 → HTML 字符串） ----

  // 频道条定义（spec 字面值，顺序不可打乱）。id 全小写下划线；label 为 UI 标签。
  const TRANSFER_CHANNELS = [
    { id: 'all', label: '全部' },
    { id: 'uploading', label: '上传中' },
    { id: 'downloading', label: '下载中' },
    { id: 'cloud_tasks', label: '云任务' },
    { id: 'cloud_groups', label: '云组' },
    { id: 'sync', label: '同步' },
    { id: 'completed', label: '已完成' },
  ];

  // 频道谓词：predicate(item) → boolean（供 filterTransferItems 分发）。
  // 语义与 spec 分节 1 一致：uploading 仅 upload 类（hashing/uploading/paused/failed/cancelled）；
  // downloading 仅 download 类（含 archive）；cloud_tasks/cloud_groups 按 kind 全量透传；
  // completed 按 status==='completed' 全 kind 命中。
  const _channelPredicates = {
    all: function () { return true; },
    uploading: function (it) { return it.kind === 'upload' && ['hashing', 'uploading', 'paused', 'failed', 'cancelled'].indexOf(it.status) >= 0; },
    downloading: function (it) { return it.kind === 'download' && ['downloading', 'paused', 'failed', 'cancelled'].indexOf(it.status) >= 0; },
    cloud_tasks: function (it) { return it.kind === 'cloud_task'; },
    cloud_groups: function (it) { return it.kind === 'cloud_group'; },
    sync: function (it) { return it.kind === 'sync_task' && ['pending', 'syncing', 'completed', 'failed', 'cancelled'].indexOf(it.status) >= 0; },
    completed: function (it) { return it.status === 'completed'; },
  };

  // filterTransferItems(items, channel)：按频道筛选，顺序保留；
  // channel 缺省（null/undefined）→ all；未知/空字符串频道 fail-closed 返回 []。
  function filterTransferItems(items, channel) {
    const list = Array.isArray(items) ? items : [];
    if (channel == null) channel = 'all';
    const pred = _channelPredicates[channel];
    if (!pred) return [];
    return list.filter(pred);
  }

  const KIND_LABEL = { upload: '上传', download: '下载', cloud_task: '云任务', cloud_group: '云组', sync_task: '同步' };
  const KIND_ICON = { upload: '⬆', download: '⬇', cloud_task: '☁', cloud_group: '🗂', sync_task: '🔄' };
  function _kindIcon(kind) { return KIND_ICON[kind] || '📄'; }
  function _kindLabel(kind) { return KIND_LABEL[kind] || kind || '项'; }

  // 主标题回退：filename || name || id || '-'（云组类用 name）。
  function _rowTitle(it) { return it.filename || it.name || it.id || '-'; }

  // 已缓存块计数（meta.chunksBitmap 置位合计）。无 bitmap → 0。
  function _cachedChunksOf(meta) {
    const b = meta && meta.chunksBitmap;
    if (!Array.isArray(b)) return 0;
    let n = 0;
    for (let i = 0; i < b.length; i++) if (b[i]) n++;
    return n;
  }

  // 云任务操作按钮组（复用既有事件委托类名，data-* 携带展示 id/data-filename/data-checksum）。
  // 状态机：completed → 下载到本地 + 删除；failed/cancelled → 恢复 + 删除；pending/downloading
  // → 取消。展示 id = 'cloud-task-' + 服务端真实 id；委托侧经 app.js stripCloudId 剥前缀后
  // 调 sc.cloud.*（真实 id 形如 cloud-<8hex>-<seq>）。
  function _cloudTaskActions(it) {
    const st = it.status;
    const raw = (it.meta && it.meta.raw) || {};
    const id = it.id;
    const filename = it.filename || raw.filename || '';
    const checksum = raw.checksum || it.checksum || '';
    let a = '';
    if (st === 'completed') {
      a += '<button class="btn btn-primary btn-sm cloud-download-btn" data-id="' + escHtml(id) + '" data-filename="' + escHtml(filename) + '" data-checksum="' + escHtml(checksum) + '" style="margin-right:4px;">下载到本地</button>';
      a += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
    } else if (st === 'failed' || st === 'cancelled') {
      a += '<button class="btn btn-sm btn-secondary cloud-resume-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">恢复</button>';
      a += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
    } else {
      // pending / downloading：取消（下载中取消语义正确）。
      a += '<button class="btn btn-warning btn-sm cloud-cancel-btn" data-id="' + escHtml(id) + '">取消</button>';
    }
    return a;
  }

  // 云组操作按钮组：completed → 打包；failed/cancelled → 恢复；pending/downloading → 取消；
  // 任意状态都提供删除 + 展开/收起。展开行 id 为 group-detail-<displayID>（由 buildTransferRowHtml
  // 输出），API 调用侧剥 'cloud-group-' 前缀；展示 id 保留前缀以避撞 localStorage 项 id。
  function _cloudGroupActions(it) {
    const st = it.status;
    const id = it.id;
    let a = '';
    if (st === 'completed') {
      a += '<button class="btn btn-primary btn-sm group-archive-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">打包</button>';
    }
    if (st === 'failed' || st === 'cancelled') {
      a += '<button class="btn btn-sm btn-secondary group-resume-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">恢复</button>';
    }
    if (st === 'pending' || st === 'downloading') {
      a += '<button class="btn btn-warning btn-sm group-cancel-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">取消</button>';
    }
    a += '<button class="btn btn-danger btn-sm group-delete-btn" data-id="' + escHtml(id) + '">删除</button>';
    a += '<button class="btn btn-small group-toggle-btn" data-id="' + escHtml(id) + '">展开</button>';
    return a;
  }

  // 同步任务操作按钮组：进行中（pending/syncing）取消 + 刷新；终态（completed/failed/cancelled）
  // 删除 + 刷新。data-id 即服务端任务 id（sync 任务不入 localStorage，无展示前缀——与
  // cloud-task-/cloud-group- 不同，事件委托侧无需剥前缀）。
  function _syncTaskActions(it) {
    const st = it.status;
    const id = it.id;
    let a = '';
    if (st === 'pending' || st === 'syncing') {
      a += '<button class="btn btn-warning btn-sm sync-cancel-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">取消</button>';
    } else if (st === 'completed' || st === 'failed' || st === 'cancelled') {
      a += '<button class="btn btn-danger btn-sm sync-delete-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">删除</button>';
    }
    a += '<button class="btn btn-sm btn-secondary sync-refresh-btn" data-id="' + escHtml(id) + '">刷新</button>';
    return a;
  }

  // 操作按钮组（data-* 携带：item-id + 状态，按钮类名沿用 .btn/.btn-sm + transfer-* 语义类）。
  // 状态机：
  //   hashing/uploading/downloading/pending → 暂停 + 取消
  //   paused → 恢复 + 取消（upload/download 类恢复，云行暂停仅取消）
  //   failed/cancelled（upload/download 类）→ 恢复 + 删除记录
  //   completed → 完成专属（打开目录/重新下载）+ 删除记录
  //   cloud_* → 云专享按钮组（kind 分派 _cloudTaskActions / _cloudGroupActions），非通用 transfer-*
  function _rowActions(it) {
    const idHtml = ' data-item-id="' + escHtml(it.id) + '"';
    const st = it.status;
    const kind = it.kind;
    let a = '';
    if (kind === 'cloud_task') { a = _cloudTaskActions(it); return a.length ? a : idHtml; }
    if (kind === 'cloud_group') { a = _cloudGroupActions(it); return a.length ? a : idHtml; }
    if (kind === 'sync_task') { a = _syncTaskActions(it); return a.length ? a : idHtml; }
    if (st === 'completed') {
      if (kind === 'upload') {
        a += '<button class="btn btn-sm btn-primary transfer-open-dir-btn" data-item-id="' + escHtml(it.id) + '">打开存储目录</button>';
      } else if (kind === 'download') {
        a += '<button class="btn btn-sm btn-primary transfer-redownload-btn" data-item-id="' + escHtml(it.id) + '">重新下载</button>';
      }
      a += '<button class="btn btn-danger btn-sm transfer-delete-btn" data-item-id="' + escHtml(it.id) + '">删除记录</button>';
    } else if (st === 'paused') {
      // 暂停（可恢复）：恢复 + 取消。
      if (kind === 'upload' || kind === 'download') {
        a += '<button class="btn btn-sm btn-secondary transfer-resume-btn" data-item-id="' + escHtml(it.id) + '">恢复</button>';
      }
      a += '<button class="btn btn-warning btn-sm transfer-cancel-btn" data-item-id="' + escHtml(it.id) + '">取消</button>';
    } else if (st === 'failed' || st === 'cancelled') {
      // 终态失败/取消：upload/download 可恢复；一律可删记录（failed 仍给取消已无意义——删除记录）
      if ((st === 'failed' || st === 'cancelled') && (kind === 'upload' || kind === 'download')) {
        a += '<button class="btn btn-sm btn-secondary transfer-resume-btn" data-item-id="' + escHtml(it.id) + '">恢复</button>';
      }
      a += '<button class="btn btn-danger btn-sm transfer-delete-btn" data-item-id="' + escHtml(it.id) + '">删除记录</button>';
    } else if (kind === 'cloud_task' || kind === 'cloud_group') {
      // 云类非终态：取消（终态 completed/failed/cancelled 已在上两支覆盖）
      a += '<button class="btn btn-warning btn-sm transfer-cancel-btn" data-item-id="' + escHtml(it.id) + '">取消</button>';
      a += '<button class="btn btn-danger btn-sm transfer-delete-btn" data-item-id="' + escHtml(it.id) + '">删除</button>';
    } else {
      // 进行中（upload/download）：暂停 + 取消。
      a += '<button class="btn btn-sm btn-secondary transfer-pause-btn" data-item-id="' + escHtml(it.id) + '">暂停</button>';
      a += '<button class="btn btn-warning btn-sm transfer-cancel-btn" data-item-id="' + escHtml(it.id) + '">取消</button>';
    }
    return a.length ? a : idHtml; // 未知状态兜底至少可寻址
  }

  // sync 进度：进行中（pending/syncing）显示字节进度条（bytes_total>0）或文件计数进度串。
  function _syncProgressHtml(it) {
    const st = it.status;
    if (st !== 'syncing' && st !== 'pending') return '';
    const meta = buildSyncRowMeta(it);
    if (meta.hasBytes) return buildProgressBar(meta.loaded, meta.total);
    if (meta.hasFiles) return '<span style="font-size:12px;color:var(--text-secondary);white-space:nowrap;">' + meta.progressText + '</span>';
    return '';
  }

  // 进度条/百分比（复用 buildProgressBar 当 total>0 与进行中状态；否则 ''）。
  function _progressHtml(it) {
    const st = it.status;
    if (it.kind === 'sync_task') return _syncProgressHtml(it);
    const total = it.total > 0 ? it.total : 0;
    if (total <= 0) return '';
    if (st === 'uploading' || st === 'downloading' || st === 'pending' || st === 'hashing') {
      return buildProgressBar(it.loaded, total);
    }
    return '';
  }

  // 统一行：kind 图标 + filename + 状态徽章(statusText) + 进度条/百分比 + 操作按钮组。
  // 上传含「已缓存 X/Y 块」；下载含「重新下载」（completed）；completed 折叠由 list 处理。
  // 云组：主标题用 name；追加可展开/收起的子任务详情行（id 为 group-detail-<displayID>，由
  // _cloudGroupActions 的「展开」按钮触发 toggleGroupTasks）。展示 id 保留前缀，API 侧剥前缀。
  function buildTransferRowHtml(item) {
    item = item || {};
    const kind = item.kind || '';
    const title = _rowTitle(item);
    const titleHtml = kind === 'cloud_group' ? (item.name || item.id || '-') : title;
    const badgeText = kind === 'sync_task' ? syncStatusText(item.status) : statusText(item.status);
    // 审查 M-1：状态文本来自服务端，转义防注入（纵深防御；sync 行至少转义）。
    const badge = '<span style="font-size:12px;font-weight:600;margin-left:8px;padding:1px 8px;border-radius:10px;background:var(--bg-hover);color:var(--text-secondary);white-space:nowrap;">' + escHtml(badgeText) + '</span>';
    const cached = _cachedChunksOf(item.meta);
    const totalChunks = item.meta && item.meta.totalChunks ? item.meta.totalChunks : 0;
    const cachedHtml = cached > 0 ? '<span style="font-size:11px;color:var(--text-muted);margin-left:8px;">已缓存 ' + cached + '/' + totalChunks + ' 块</span>' : '';
    const actions = _rowActions(item);
    const detail = kind === 'cloud_group'
      ? '<div id="group-detail-' + escHtml(item.id) + '" style="display:none;padding:0 12px 12px 44px;background:var(--bg-page);"><div class="group-task-list" style="padding:8px;font-size:13px;">加载中...</div></div>'
      : '';
    return '<div class="transfer-row" data-item-id="' + escHtml(item.id) + '" style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid var(--border-color);background:var(--bg-container);">' +
      '<span style="font-size:16px;">' + _kindIcon(kind) + '</span>' +
      '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(title) + '">' + escHtml(titleHtml) + badge + cachedHtml + '</span>' +
      '<span style="white-space:nowrap;">' + _progressHtml(item) + '</span>' +
      '<span class="transfer-actions" style="white-space:nowrap;">' + actions + '</span>' +
      '</div>' + detail;
  }

  // 已完成折叠分组行（summary）：组标签 + 计数；detail id 为 group-detail-<kind>。
  function _groupSummaryHtml(kind, count, detailId) {
    return '<details style="border-bottom:1px solid var(--border-color);"><summary style="cursor:pointer;padding:10px 12px;background:var(--bg-container);color:var(--text-secondary);font-size:13px;">' +
      '✅ 已完成' + _kindLabel(kind) + ' (' + count + ')' +
      '<span style="float:right;font-size:11px;">点击展开</span></summary>' +
      '<div class="group-detail" id="' + detailId + '" style="background:var(--bg-page);">';
  }

  function _completedGroupsHtml(completedItems) {
    const orderedKinds = ['upload', 'download', 'cloud_task', 'cloud_group', 'sync_task'];
    const grouped = {};
    for (const it of completedItems || []) {
      const k = it.kind || 'other';
      (grouped[k] = grouped[k] || []).push(it);
    }
    let html = '';
    for (const k of orderedKinds) {
      const group = grouped[k] || [];
      if (group.length === 0) continue;
      html += _groupSummaryHtml(k, group.length, 'group-detail-' + k);
      html += group.map(buildTransferRowHtml).join('');
      html += '</div></details>';
    }
    return html;
  }

  // buildTransferListHtml(items, channel)：过滤 + 已完成按 kind 分组折叠 + 空文案。
  // 无完成项时整个列表平铺（避免空 details 占行）；有完成项时完成后置并折叠。
  function buildTransferListHtml(items, channel) {
    const filtered = filterTransferItems(items, channel);
    if (filtered.length === 0) return '<div class="empty-msg">暂无传输记录</div>';
    const completed = filtered.filter(function (it) { return it.status === 'completed'; });
    if (completed.length === 0) {
      return filtered.map(buildTransferRowHtml).join('');
    }
    const running = filtered.filter(function (it) { return it.status !== 'completed'; });
    return running.map(buildTransferRowHtml).join('') + _completedGroupsHtml(completed);
  }

  return {
    escHtml, formatSize, getChecksumPrefix, bytesToHex, normalizeList, zipNames,
    stripCloudId,
    uploadProgressText,
    parseCloudLines, previewKind, buildFileTableHtml, buildFileRowHtml,
    buildLoadMoreHtml, buildAllLoadedHtml, hubTableHtml, configTableHtml, statsTableHtml,
    statusText, buildProgressBar, cloudTaskActions, buildCloudTaskTableHtml,
    cloudGroupActions, buildCloudGroupTableHtml, buildVersionTableHtml,
    syncStatusText, buildSyncRowMeta,
    TRANSFER_CHANNELS, filterTransferItems, buildTransferRowHtml, buildTransferListHtml,
  };
});
