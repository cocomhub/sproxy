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

  function cloudTaskActions(id, filename, status, checksum) {
    let actions = '';
    if (status === 'completed') {
      actions += '<button class="btn btn-primary btn-sm cloud-download-btn" data-id="' + escHtml(id) + '" data-filename="' + escHtml(filename) + '" data-checksum="' + escHtml(checksum || '') + '" style="margin-right:4px;">下载到本地</button>';
      actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
    } else if (status === 'failed' || status === 'cancelled') {
      actions += '<button class="btn btn-sm btn-secondary cloud-resume-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">恢复</button>';
      actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
    } else {
      actions += '<button class="btn btn-warning btn-sm cloud-cancel-btn" data-id="' + escHtml(id) + '">取消</button>';
    }
    return actions;
  }

  function buildCloudTaskTableHtml(tasks) {
    let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">文件名</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">大小</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';
    for (const t of tasks || []) {
      const statusLabel = statusText(t.status);
      const rowClass = t.status === 'downloading' ? ' style="background:var(--bg-auth);"' : '';
      const progressHtml = t.status === 'downloading' && t.total_size > 0 ? buildProgressBar(t.downloaded, t.total_size) : '';
      html += '<tr' + rowClass + '><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(t.filename || '') + '">' + escHtml(t.filename || '-') + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + statusLabel + progressHtml + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' + (t.total_size > 0 ? formatSize(t.total_size) : '-') + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' +
        cloudTaskActions(t.id, t.filename, t.status, t.checksum) + '</td></tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  function cloudGroupActions(id, status) {
    let actions = '';
    if (status === 'completed') {
      actions += '<button class="btn btn-primary btn-sm group-archive-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">打包</button>';
    }
    if (status === 'failed' || status === 'cancelled') {
      actions += '<button class="btn btn-sm btn-secondary group-resume-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">恢复</button>';
    }
    if (status === 'pending' || status === 'downloading') {
      actions += '<button class="btn btn-warning btn-sm group-cancel-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">取消</button>';
    }
    actions += '<button class="btn btn-danger btn-sm group-delete-btn" data-id="' + escHtml(id) + '">删除</button>';
    return actions;
  }

  function buildCloudGroupTableHtml(groups) {
    let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">名称</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">进度</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th>' +
      '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);"></th></tr></thead><tbody>';
    for (const g of groups || []) {
      const statusLabel = statusText(g.status);
      const progressText = g.completed + '/' + g.total_tasks;
      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(g.name || g.id) + '">' + escHtml(g.name || g.id) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + statusLabel + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + progressText + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);white-space:nowrap;">' +
        cloudGroupActions(g.id, g.status) + '</td>' +
        '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);"><button class="btn btn-small group-toggle-btn" data-id="' + escHtml(g.id) + '">展开</button></td></tr>' +
        '<tr id="group-detail-' + escHtml(g.id) + '" style="display:none;"><td colspan="5"><div class="group-task-list" style="padding:8px;font-size:13px;">加载中...</div></td></tr>';
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

  return {
    escHtml, formatSize, getChecksumPrefix, bytesToHex, normalizeList, zipNames,
    uploadProgressText,
    parseCloudLines, previewKind, buildFileTableHtml, buildFileRowHtml,
    buildLoadMoreHtml, buildAllLoadedHtml, hubTableHtml, configTableHtml, statsTableHtml,
    statusText, buildProgressBar, cloudTaskActions, buildCloudTaskTableHtml,
    cloudGroupActions, buildCloudGroupTableHtml, buildVersionTableHtml,
  };
});
