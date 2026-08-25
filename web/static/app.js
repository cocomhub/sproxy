// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 主逻辑：文件列表、CRUD、批量操作、导航、UI 工具。
// 依赖 sha256.js, sclient/*, cloudfilename.js, upload.js（先加载）。

const BASE = '';
// SproxySig 请求签名认证（AccessKey/AccessKeySecret）。Secret 只存本端计算签名，
// 永不上线；线上请求只携带 AccessKey + HMAC 签名。存 sessionStorage（关页即清）。
let accessKey = sessionStorage.getItem('sproxy_access_key') || '';
let accessKeySecret = sessionStorage.getItem('sproxy_access_key_secret') || '';
let currentSubdir = localStorage.getItem('sproxy_subdir') || '';
let _searchActive = false;
let _currentOffset = 0;
let _hasMore = false;
const PAGE_LIMIT = 500;

// 传输层由 sclient 领域库统一（隧道/直连协商 + SproxySig 签名），页面级
// sc 常量在底部 sclientInit 中按 cuTunnelOverride 注入后创建。
let sc = null;

// cuTunnelOverride 读取「走隧道（调试）」checkbox 的持久化值。
// 读写 localStorage 键 sproxy_web_transport_override（与 sclient config.js 的
// overrideKey 同键）——transport.readLocalOverride/effectiveMode 实时读它。
const CUTUNNEL_OVERRIDE_KEY = 'sproxy_web_transport_override';
function cuTunnelOverride() {
  try {
    const v = localStorage.getItem(CUTUNNEL_OVERRIDE_KEY);
    return v === 'tunnel';
  } catch { return false; }
}

// sclientInit 创建领域 API 并落地服务端 web.tunnel 开关。
// 在 DOMContentLoaded 之前执行（脚本按顺序加载，此时 DOM 元素尚未就绪——
// checkbox 仅由 DOMContentLoaded 中的 checkboxInit 读取，本处不触碰 DOM）。
function sclientInit() {
  // 先把页面/localStorage 的传输 override 注入 transport，再创建领域 API。
  sclientTransport.configure({
    accessKey: accessKey,
    accessKeySecret: accessKeySecret,
  });
  sc = sclientApi.apiFromGlobals();

  // 落地服务端 web.tunnel 默认值（GET /api/config 下发），失败静默（直接模式仍可用）。
  sc.config.get().then(function(data) {
    sclientTransport.configure({ accessKey: accessKey, accessKeySecret: accessKeySecret, tunnelDefault: !!data.web_tunnel });
  }).catch(function() { /* 忽略——无 web.tunnel 存取时保持默认 */ });
}

// checkboxInit 初始化「走隧道（调试）」开关：初值从 localStorage 读取。
// 保存按钮仍写 sessionStorage（AK/SK）。保存后同步到 transport（VP-1：sc 用当前 AK/SK）。
document.addEventListener('DOMContentLoaded', function() {
  const cb = document.getElementById('use-tunnel-checkbox');
  if (!cb) return;
  cb.checked = cuTunnelOverride();
  cb.addEventListener('change', function() { toggleTransport(cb); });
});

// 还原网络传输层：检测 override，返回当前 scope 的传输配置分量。
function applyTransportOverride() {
  const override = sclientConfig.readLocalOverride();
  const ov = override && override.transport;
  const mode = (ov === 'tunnel' || ov === 'direct') ? ov : overrideTransportFallback();
  sclientTransport.configure({
    accessKey: accessKey,
    accessKeySecret: accessKeySecret,
    mode: mode,
    tunnelDefault: undefined,
  });
  return mode;
}

// overrideTransportFallback 回退读取 transport 的有效模式（override 为空时按服务端开关）。
function overrideTransportFallback() {
  return (sclientTransport.effectiveMode && sclientTransport.effectiveMode() === 'tunnel') ? 'tunnel' : 'direct';
}

// refreshTransport 复算传输配置（切换 override / 保存 AK/SK / 服务端开关后调用）。
function refreshTransport() {
  try {
    applyTransportOverride();
  } catch (e) { /* ignore */ }
}

// toggleTransport 读写「走隧道（调试）」checkbox，并落地 override + 强制 mode 即时生效。
function toggleTransport(cb) {
  const on = !!cb.checked;
  // 写到 localStorage override（transport.readLocalOverride/effectiveMode 读取，
  // auto 时有效）。显式 'tunnel'/'direct' 同时强制 transport.mode 即时生效。
  try {
    if (on) localStorage.setItem('sproxy_web_transport_override', 'tunnel');
    else localStorage.setItem('sproxy_web_transport_override', 'direct');
    sclientTransport.configure({
      accessKey: accessKey,
      accessKeySecret: accessKeySecret,
      mode: on ? 'tunnel' : 'direct',
      tunnelDefault: undefined,
    });
  } catch (e) { /* ignore */ }
  return on;
}

document.getElementById('accessKey').value = accessKey;
document.getElementById('token').value = accessKeySecret;

function saveAccessKeys() {
  accessKey = document.getElementById('accessKey').value.trim();
  accessKeySecret = document.getElementById('token').value.trim();
  sessionStorage.setItem('sproxy_access_key', accessKey);
  sessionStorage.setItem('sproxy_access_key_secret', accessKeySecret);
  // 同步到 transport（sc 已创建时；含 override/默认沿用）
  try {
    sclientTransport.configure({ accessKey: accessKey, accessKeySecret: accessKeySecret, mode: undefined, tunnelDefault: undefined });
  } catch (e) { /* ignore */ }
  showToast('AccessKey 已保存', 'success');
}

// --- UI 工具 ---
function showToast(msg, type) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast toast-' + type + ' show';
  clearTimeout(el._timer);
  el._timer = setTimeout(function() { el.classList.remove('show'); }, 3000);
}

function formatSize(bytes) {
  if (bytes >= 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB';
  if (bytes >= 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return bytes + ' B';
}

function escHtml(s) {
  return String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
}

// --- SproxySig 请求签名（WebCrypto，与 Go pkg/sproxysig 对齐） ---

// 本地自定义 SHA-256/HMAC 辅助已迁移到 sclient/crypto.js（sclientCrypto）；
// SproxySig 签名由 sclient/sig.js 统一负责，此处不再重复实现。

function getChecksumPrefix(cs) {
  if (!cs) return '-';
  return cs.substring(0, 16) + '…';
}

function copyChecksum(cs) {
  navigator.clipboard.writeText(cs).then(function() {
    showToast('Checksum 已复制到剪贴板', 'success');
  }).catch(function() {
    showToast('复制失败', 'error');
  });
}

// --- 文件列表 ---
async function refreshList() {
  const el = document.getElementById('file-list');
  el.innerHTML = '<div class="empty-msg">加载中...</div>';
  updateBreadcrumb();
  _currentOffset = 0;
  _hasMore = false;
  try {
    let data = await sc.files.list(currentSubdir, { offset: 0, limit: PAGE_LIMIT });
    let files = Array.isArray(data) ? data : (data && data.files) || [];
    _currentOffset = files.length;
    _hasMore = (data.total || 0) > _currentOffset;
    if (files.length === 0) { el.innerHTML = '<div class="empty-msg">暂无文件</div>'; return; }
    el.innerHTML = buildFileTableHtml(files, currentSubdir) + buildLoadMoreHtml(data.total);
    updateBatchToolbar();
  } catch (e) {
    el.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  }
}

async function loadMore() {
  const el = document.getElementById('file-list');
  const qs = (currentSubdir ? '?subdir=' + encodeURIComponent(currentSubdir) + '&' : '?') + 'offset=' + _currentOffset + '&limit=' + PAGE_LIMIT;
  const listUrl = '/api/files' + qs;
  try {
    let data = await sc.files.list(currentSubdir, { offset: _currentOffset, limit: PAGE_LIMIT });
    let files = Array.isArray(data) ? data : (data && data.files) || [];
    _currentOffset += files.length;
    _hasMore = (data.total || 0) > _currentOffset;

    const tbody = el.querySelector('table tbody');
    if (!tbody) { refreshList(); return; }
    for (const fi of files) {
      const fullName = currentSubdir ? currentSubdir + '/' + fi.name : fi.name;
      tbody.insertAdjacentHTML('beforeend', buildFileRowHtml(fi, fullName));
    }
    const container = document.getElementById('load-more-container');
    if (container) {
      if (_hasMore) {
        const remaining = (data.total || 0) - _currentOffset;
        container.innerHTML = '<button class="btn btn-primary">加载更多 (' + remaining + ')</button>';
      } else {
        container.innerHTML = '<div style="text-align:center;padding:12px;color:var(--text-muted);">已加载全部 ' + data.total + ' 个文件</div>';
      }
    }
  } catch { /* 静默处理 */ }
}

function buildFileTableHtml(files, subdir) {
  let html = '<table id="file-table"><thead><tr><th class="check-col"><input type="checkbox" id="select-all-checkbox"></th><th>文件名</th><th>大小</th><th>Checksum (SHA-256)</th><th>操作</th></tr></thead><tbody>';
  for (const fi of files) {
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

function buildLoadMoreHtml(total) {
  if (!_hasMore) return '';
  const remaining = (total || 0) - _currentOffset;
  return '<div id="load-more-container" style="text-align:center;padding:12px;">' +
    '<button class="btn btn-primary">加载更多 (' + remaining + ')</button></div>';
}

// --- 搜索 ---
async function searchFiles() {
  const q = document.getElementById('search-input').value.trim();
  if (!q) { clearSearch(); return; }
  const el = document.getElementById('file-list');
  el.innerHTML = '<div class="empty-msg">搜索中...</div>';
  try {
    let data = await sc.files.search(q);
    let files = Array.isArray(data) ? data : (data && data.files) || [];
    _searchActive = true;
    document.getElementById('clear-search-btn').style.display = '';
    if (files.length === 0) { el.innerHTML = '<div class="empty-msg">未找到匹配文件</div>'; return; }
    el.innerHTML = buildFileTableHtml(files, '');
    updateBatchToolbar();
  } catch (e) {
    el.innerHTML = '<div class="empty-msg">搜索失败: ' + e.message + '</div>';
  }
}

function clearSearch() {
  document.getElementById('search-input').value = '';
  document.getElementById('clear-search-btn').style.display = 'none';
  _searchActive = false;
  refreshList();
}

// --- 目录导航 ---
function navigateDir(subdir) {
  currentSubdir = subdir;
  localStorage.setItem('sproxy_subdir', subdir);
  refreshList();
}

function updateBreadcrumb() {
  const el = document.getElementById('dir-breadcrumb');
  if (!currentSubdir) {
    el.innerHTML = '<a href="#" data-subdir="">/</a>';
    return;
  }
  const parts = currentSubdir.split('/');
  let html = '<a href="#" data-subdir="">/</a>';
  let accumulated = '';
  for (const p of parts) {
    accumulated = accumulated ? accumulated + '/' + p : p;
    html += ' <span style="color:var(--text-muted)">›</span> <a href="#" data-subdir="' + escHtml(accumulated) + '">' + escHtml(p) + '</a>';
  }
  el.innerHTML = html;
}

// --- 目录操作 ---
async function mkdirDir() {
  const input = document.getElementById('new-dir-name');
  const name = input.value.trim();
  if (!name) { showToast('请输入目录名', 'warning'); return; }
  const dirPath = currentSubdir ? currentSubdir + '/' + name : name;
  try {
    const data = await sc.files.mkdir(dirPath);
    if (data && data.success) {
      showToast('目录已创建: ' + dirPath, 'success');
      input.value = '';
      refreshList();
    } else { showToast('创建目录失败: ' + ((data && data.message) || ''), 'error'); }
  } catch (e) { showToast('创建目录失败: ' + e.message, 'error'); }
}

async function rmdirDir(dirPath) {
  if (!confirm('确认删除目录 "' + dirPath + '" 及其所有内容?')) return;
  try {
    const data = await sc.files.rmdir(dirPath);
    if (data && data.success) { showToast('目录已删除: ' + dirPath, 'success'); refreshList(); }
    else { showToast('删除目录失败: ' + ((data && data.message) || ''), 'error'); }
  } catch (e) { showToast('删除目录失败: ' + e.message, 'error'); }
}

// --- 下载 ---
// downloadFile 走 sc.files.download(name) → Blob（传输层自动协商隧道/直连）。
async function downloadFile(name, expectedChecksum) {
  try {
    const blob = await sc.files.download(name);
    triggerDownload(name, blob);
    showToast(name + ' 下载完成', 'success');
  } catch (e) { showToast('下载失败: ' + e.message, 'error'); }
}

function triggerDownload(fileName, data) {
  const blob = data instanceof Blob ? data : new Blob([data]);
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = fileName;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// --- 删除 ---
async function deleteFile(name, checksum) {
  if (!confirm('确认删除 "' + name + '"?')) return;
  if (!checksum) { showToast('缺少 checksum，无法校验完整性', 'error'); return; }
  try {
    const data = await sc.files.deleteFile(name, checksum);
    if (data && data.success) { showToast('删除成功: ' + name, 'success'); refreshList(); }
    else { showToast('删除失败: ' + ((data && data.message) || ''), 'error'); }
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

// --- 重命名 ---
async function renameFile(name, checksum) {
  if (!checksum) { showToast('缺少 checksum，无法校验完整性', 'error'); return; }
  const newName = prompt('新的文件名（路径）:', name);
  if (!newName || newName === name) return;
  try {
    const data = await sc.files.rename(name, newName, checksum);
    if (data && data.success) { showToast('重命名成功: ' + newName, 'success'); refreshList(); }
    else { showToast('重命名失败: ' + ((data && data.message) || ''), 'error'); }
  } catch (e) { showToast('重命名失败: ' + e.message, 'error'); }
}

// --- 批量操作 ---
function toggleSelectAll(checked) {
  for (const cb of document.querySelectorAll('.file-select')) { cb.checked = checked; }
  updateBatchToolbar();
}

function updateBatchToolbar() {
  const cbs = document.querySelectorAll('.file-select:checked');
  const count = cbs.length;
  const toolbar = document.getElementById('batch-toolbar');
  const label = document.getElementById('batch-count');
  if (!toolbar || !label) return;
  label.textContent = '已选 ' + count + ' 个文件';
  if (count > 0) { toolbar.classList.add('show'); } else { toolbar.classList.remove('show'); }
}

function clearSelection() {
  for (const cb of document.querySelectorAll('.file-select:checked')) { cb.checked = false; }
  updateBatchToolbar();
}

function getSelectedFiles() {
  const results = [];
  for (const cb of document.querySelectorAll('.file-select:checked')) {
    const filename = cb.dataset.filename;
    const checksum = cb.dataset.checksum;
    if (filename) results.push({ filename: filename, checksum: checksum });
  }
  return results;
}

async function batchDelete() {
  const files = getSelectedFiles();
  if (files.length === 0) { showToast('请先选择文件', 'error'); return; }
  if (!confirm('确定要删除选中的 ' + files.length + ' 个文件吗？')) return;
  try {
    const data = await sc.files.batchDelete(files);
    if (data.success) { showToast(data.message || '删除完成', 'success'); refreshList(); }
    else { showToast(data.message || '批量删除失败', 'error'); }
  } catch (e) { showToast('批量删除失败: ' + e.message, 'error'); }
}

async function batchRename() {
  const files = getSelectedFiles();
  if (files.length === 0) { showToast('请先选择文件', 'error'); return; }
  const operations = [];
  for (const f of files) {
    const newName = prompt('重命名 "' + f.filename + '"\n请输入新文件名（取消跳过）:', f.filename);
    if (newName === null) continue;
    if (newName.trim() === '') { showToast('文件名不能为空', 'error'); return; }
    if (newName === f.filename) continue;
    operations.push({ from: f.filename, to: newName, checksum: f.checksum });
  }
  if (operations.length === 0) { showToast('没有需要重命名的文件', 'info'); return; }
  try {
    const data = await sc.files.batchRename(operations);
    if (data.success) { showToast(data.message || '重命名完成', 'success'); clearSelection(); refreshList(); }
    else { showToast(data.message || '批量重命名失败', 'error'); }
  } catch (e) { showToast('批量重命名失败: ' + e.message, 'error'); }
}

async function batchDownloadArchive() {
  const selected = getSelectedFiles();
  if (selected.length === 0) { showToast('请选择文件', 'warning'); return; }
  const files = selected.map(function(f) { return f.filename; });
  try {
    const ar = await sc.files.archive(files);
    if (ar && ar.success && ar.blob) {
      triggerDownload(ar.filename || 'archive.tar.gz', ar.blob);
      showToast('归档下载完成: ' + (ar.filename || 'archive.tar.gz'), 'success');
    } else {
      showToast('归档失败: ' + ((ar && ar.message) || '未知错误'), 'error');
    }
  } catch (err) { showToast('归档失败: ' + err.message, 'error'); }
}

// 目录打包下载（GET /api/archive-dir）
async function downloadDirArchive(dirPath) {
  try {
    const ar = await sc.files.archiveDir(dirPath);
    if (ar && ar.success && ar.blob) {
      triggerDownload(ar.filename || (dirPath.replace('/', '_') + '.tar.gz'), ar.blob);
      showToast('目录打包下载完成', 'success');
    } else {
      showToast('打包下载失败: ' + ((ar && ar.message) || '未知错误'), 'error');
    }
  } catch (e) { showToast('打包下载失败: ' + e.message, 'error'); }
}

// --- 监控 ---
async function showStats() {
  document.getElementById('stats-modal').style.display = 'flex';
  switchStatsTab('stats');
  document.getElementById('stats-panel').innerHTML = '<div style="text-align:center;padding:20px;color:var(--text-muted);">加载中...</div>';
  try {
    const data = await sc.statsGet();
    var du = data.disk_usage || {};
    var rc = data.request_counts || {};
    document.getElementById('stats-panel').innerHTML = statsTableHtml(du, rc, data);
  } catch (e) { document.getElementById('stats-panel').innerHTML = '<div style="color:red">错误: ' + e.message + '</div>'; }
}

function hideStats() {
  document.getElementById('stats-modal').style.display = 'none';
}

// --- 监控弹窗标签页切换 ---
function switchStatsTab(tab) {
  document.getElementById('stats-panel').style.display = tab === 'stats' ? 'block' : 'none';
  document.getElementById('config-panel').style.display = tab === 'config' ? 'block' : 'none';
  document.getElementById('hub-panel').style.display = tab === 'hub' ? 'block' : 'none';
  document.querySelectorAll('.stats-tab').forEach(function(el) {
    el.style.borderBottomColor = el.id === tab + '-tab' ? 'var(--tab-active)' : 'transparent';
    el.style.color = el.id === tab + '-tab' ? 'var(--text-primary)' : 'var(--text-secondary)';
  });
  if (tab === 'config') showConfig();
  if (tab === 'hub') showHub();
}

async function showConfig() {
  document.getElementById('config-panel').innerHTML = '<div style="text-align:center;padding:20px;color:var(--text-muted);">加载中...</div>';
  try {
    const data = await sc.config.get();
    document.getElementById('config-panel').innerHTML = configTableHtml(data);
  } catch (e) { document.getElementById('config-panel').innerHTML = '<div style="color:red">错误: ' + e.message + '</div>'; }
}

// --- Hub 管理 ---
async function showHub() {
  document.getElementById('hub-panel').innerHTML = '<div style="text-align:center;padding:20px;color:var(--text-muted);">加载中...</div>';
  try {
    const [nData, sData] = await Promise.all([
      sc.hub.nodes(),
      sc.hub.stats(),
    ]);
    const nodes = Array.isArray(nData) ? nData : ((nData && nData.nodes) || []);
    const stats = sData;
    document.getElementById('hub-panel').innerHTML = hubTableHtml(nodes, stats);
  } catch (e) {
    document.getElementById('hub-panel').innerHTML = '<div class="empty-msg">Hub 未启用或请求失败: ' + e.message + '</div>';
  }
}

function hubTableHtml(nodes, stats) {
  var html = '';
  // 统计概要
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

async function removeHubNode(nodeId) {
  if (!confirm('确定移除节点 ' + nodeId + '？')) return;
  try {
    await sc.hub.remove(nodeId);
    showToast('节点 ' + nodeId + ' 已移除', 'success');
    showHub();
  } catch (e) { showToast('移除失败: ' + e.message, 'error'); }
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
  html += row('隧道密钥', cfg.tunnel_key_set ? '✅ 已设置' : '❌ 未设置');
  html += row('速率限制', cfg.rate_limit_requests + ' req / ' + (cfg.rate_limit_window || '-'));
  html += row('存储上限', cfg.max_storage_bytes > 0 ? formatBytes(cfg.max_storage_bytes) : '不限');
  html += row('分块大小', formatBytes(cfg.chunk_size));
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

  // 日志级别
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:var(--text-secondary);">日志级别:</span>';
  html += '<select id="cfg-log-level" style="padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
  var levels = ['debug','info','warn','error'];
  for (var i = 0; i < levels.length; i++) {
    html += '<option value="' + levels[i] + '"' + (cfg.log_level === levels[i] ? ' selected' : '') + '>' + levels[i] + '</option>';
  }
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-level">更新</button></div>';

  // 日志格式
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:var(--text-secondary);">日志格式:</span>';
  html += '<select id="cfg-log-format" style="padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
  html += '<option value="text"' + (cfg.log_format === 'text' ? ' selected' : '') + '>text</option>';
  html += '<option value="json"' + (cfg.log_format === 'json' ? ' selected' : '') + '>json</option>';
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-format">更新</button></div>';

  // 速率限制
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:var(--text-secondary);">速率限制:</span>';
  html += '<input type="number" id="cfg-rate-limit" value="' + (cfg.rate_limit_requests ?? 10) + '" style="width:60px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
  html += '<span style="font-size:12px;color:var(--text-muted);">req / </span>';
  html += '<input type="text" id="cfg-rate-window" value="' + (cfg.rate_limit_window || '1s') + '" style="width:60px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;">';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-rate-limit">更新</button></div>';

  // 存储限制
  html += '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:var(--text-secondary);">存储上限:</span>';
  html += '<input type="number" id="cfg-max-storage" value="' + (cfg.max_storage_bytes ?? 0) + '" style="width:140px;padding:4px 8px;border:1px solid var(--border-input);border-radius:4px;font-size:13px;" min="0">';
  html += '<span style="font-size:12px;color:var(--text-muted);">字节（0=不限）</span>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-storage">更新</button></div>';

  html += '</div>';
  return html;
}

async function updateConfigField(key, value) {
  var patch = (function() { var o = {}; o[key] = value; return o; })();
  try {
    const data = await sc.config.update(patch);
    if (data && data.success) { showToast('配置已更新', 'success'); showConfig(); }
    else { showToast('更新失败', 'error'); }
  } catch (e) { showToast('更新失败: ' + e.message, 'error'); }
}

// async function updateStorageConfig 已废弃：存储上限统一走配置面板 cfg-update-storage
//（updateConfigField('max_storage_bytes', ...) → PUT /api/config）。旧 PUT /api/storage/config
// 无服务端路由，已由任务 5 裁定删除（config.updateStorage 移除）。本函数移除。
function formatBytes(n) {
  if (n == null) return '-';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
  return (n / 1073741824).toFixed(2) + ' GB';
}

function statsTableHtml(du, rc, s) {
  return '<table style="width:100%;border-collapse:collapse;font-size:14px;">' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary)">磁盘使用</th></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">目录</td><td style="text-align:right">' + (du.uploads_dir || '-') + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">文件数</td><td style="text-align:right">' + (du.total_files ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">总大小</td><td style="text-align:right">' + formatBytes(du.total_size) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary);padding-top:14px">请求统计（自启动）</th></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">总请求数</td><td style="text-align:right">' + (rc.total ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">2xx</td><td style="text-align:right">' + (rc['2xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">4xx</td><td style="text-align:right">' + (rc['4xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">5xx</td><td style="text-align:right">' + (rc['5xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">活跃连接</td><td style="text-align:right">' + (s.active_connections ?? 0) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid var(--border-color);color:var(--text-secondary);padding-top:14px">传输统计（自启动）</th></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">上传文件数</td><td style="text-align:right">' + (s.files_uploaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">上传字节数</td><td style="text-align:right">' + formatBytes(s.bytes_uploaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">下载文件数</td><td style="text-align:right">' + (s.files_downloaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">下载字节数</td><td style="text-align:right">' + formatBytes(s.bytes_downloaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:var(--text-secondary)">删除文件数</td><td style="text-align:right">' + (s.files_deleted ?? 0) + '</td></tr></table>';
}

// --- 暗色模式 ---
function initTheme() {
  var saved = localStorage.getItem('sproxy_theme');
  if (saved === 'dark') {
    document.documentElement.setAttribute('data-theme', 'dark');
    document.getElementById('theme-toggle-btn').textContent = '☀️';
  } else if (saved === 'light') {
    document.documentElement.removeAttribute('data-theme');
    document.getElementById('theme-toggle-btn').textContent = '🌙';
  } else {
    // 未保存时跟随系统
    if (window.matchMedia('(prefers-color-scheme: dark)').matches) {
      document.getElementById('theme-toggle-btn').textContent = '☀️';
    }
  }
}

function toggleTheme() {
  var current = document.documentElement.getAttribute('data-theme');
  if (current === 'dark') {
    document.documentElement.removeAttribute('data-theme');
    localStorage.setItem('sproxy_theme', 'light');
    document.getElementById('theme-toggle-btn').textContent = '🌙';
  } else {
    document.documentElement.setAttribute('data-theme', 'dark');
    localStorage.setItem('sproxy_theme', 'dark');
    document.getElementById('theme-toggle-btn').textContent = '☀️';
  }
}

// --- 键盘快捷键 ---
document.addEventListener('keydown', function(e) {
  // 忽略输入框内的快捷键
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA' || e.target.tagName === 'SELECT') return;

  switch (e.key) {
    case 'u': case 'U':
      // u: 上传文件
      e.preventDefault();
      document.getElementById('file-input').click();
      break;
    case 'r': case 'R':
      // r: 刷新列表（非 Ctrl+R）
      if (!e.ctrlKey && !e.metaKey) {
        e.preventDefault();
        refreshList();
      }
      break;
    case '/':
      // /: 搜索框聚焦
      e.preventDefault();
      document.getElementById('search-input').focus();
      break;
    case 'Escape':
      // Esc: 关闭所有弹窗
      hideStats();
      hideCloudDownload();
      hideVersioning();
      hideShareModal();
      break;
  }
});

// Ctrl+A: 全选/取消全选
document.addEventListener('keydown', function(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'a') {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    var selectAll = document.getElementById('select-all-checkbox');
    if (selectAll) {
      e.preventDefault();
      selectAll.click();
    }
  }
});

// Delete: 批量删除选中文件
document.addEventListener('keydown', function(e) {
  if (e.key === 'Delete' && !e.target.tagName.match(/INPUT|TEXTAREA|SELECT/i)) {
    var batchDelete = document.getElementById('batch-delete-btn');
    if (batchDelete && batchDelete.style.display !== 'none') {
      e.preventDefault();
      batchDelete.click();
    }
  }
});

// --- 初始化 ---
sclientInit();
initTheme();
refreshList();
checkResumableUploads();

// --- 文件分享（旧版，改用弹窗） ---
function shareFile(name) {
  var ttl = prompt('分享有效期（例如 1h, 24h, 7d，留空=24h）:', '24h');
  if (ttl === null) return;
  ttl = ttl.trim() || '24h';
  var maxDownloads = prompt('最大下载次数（0=不限）:', '0');
  if (maxDownloads === null) return;
  maxDownloads = Number.parseInt(maxDownloads) || 0;
  var oneTime = confirm('一次性分享（下载一次后自动失效）？\n确定=是，取消=否');
  (async function() {
    try {
      const data = await sc.share.create({ filename: name, ttl: ttl, max_downloads: maxDownloads, one_time: oneTime });
      if (!data.token) { showToast('创建分享失败: ' + ((data && data.message) || 'unknown'), 'error'); return; }
      var shareUrl = location.origin + '/s/' + data.token;
      if (navigator.clipboard) {
        await navigator.clipboard.writeText(shareUrl);
        showToast('分享链接已复制到剪贴板: ' + shareUrl, 'success');
      } else {
        showToast('分享链接: ' + shareUrl, 'success');
      }
    } catch (e) { showToast('创建分享失败: ' + e.message, 'error'); }
  })();
}

// --- 分享管理 ---
var _shareModalVisible = false;

function showShareModal(name) {
  _shareModalVisible = true;
  document.getElementById('share-modal').style.display = 'flex';
  document.getElementById('share-filename').value = name || '';
  document.getElementById('share-ttl').value = '24h';
  document.getElementById('share-max-downloads').value = '0';
  document.getElementById('share-one-time').checked = false;
  switchShareTab('create');
  refreshShareList();
}

function hideShareModal() {
  _shareModalVisible = false;
  document.getElementById('share-modal').style.display = 'none';
}

function switchShareTab(tab) {
  document.getElementById('share-create-panel').style.display = tab === 'create' ? 'block' : 'none';
  document.getElementById('share-list-panel').style.display = tab === 'list' ? 'block' : 'none';
  document.querySelectorAll('.share-tab').forEach(function(el) {
    el.style.borderBottomColor = el.id === 'share-' + tab + '-tab' ? 'var(--tab-active)' : 'transparent';
    el.style.color = el.id === 'share-' + tab + '-tab' ? 'var(--text-primary)' : 'var(--text-secondary)';
  });
}

async function createShare() {
  var filename = document.getElementById('share-filename').value.trim();
  if (!filename) { showToast('请输入文件名', 'error'); return; }
  var ttl = document.getElementById('share-ttl').value.trim() || '24h';
  var maxDownloads = Number.parseInt(document.getElementById('share-max-downloads').value) || 0;
  var oneTime = document.getElementById('share-one-time').checked;

  try {
    const data = await sc.share.create({ filename: filename, ttl: ttl, max_downloads: maxDownloads, one_time: oneTime });
    if (!data.token) { showToast('创建分享失败: ' + ((data && data.message) || 'unknown'), 'error'); return; }
    var shareUrl = location.origin + '/s/' + data.token;
    if (navigator.clipboard) {
      try {
        await navigator.clipboard.writeText(shareUrl);
        showToast('分享链接已复制到剪贴板: ' + shareUrl, 'success');
      } catch (_) {
        showToast('分享链接: ' + shareUrl, 'success');
      }
    } else {
      showToast('分享链接: ' + shareUrl, 'success');
    }
    refreshShareList();
  } catch (e) { showToast('创建分享失败: ' + e.message, 'error'); }
}

async function refreshShareList() {
  if (!_shareModalVisible) return;
  var body = document.getElementById('share-list-body');
  try {
    const listData = await sc.share.list();
    var shares = (listData && listData.shares) || [];

    if (shares.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无分享链接</div>';
      return;
    }

    var html = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:var(--bg-hover);"><th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">文件名</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">状态</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">下载次数</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">过期时间</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';

    for (var i = 0; i < shares.length; i++) {
      var s = shares[i];
      var statusText = s.expired ? '已过期' : (s.one_time ? '一次性' : '活跃');
      var statusColor = s.expired ? 'var(--text-muted)' : (s.one_time ? '#e67e22' : '#27ae60');
      var downloads = s.max_downloads > 0 ? s.downloads + '/' + s.max_downloads : s.downloads + '/∞';
      var expiresLabel = s.expired ? '-' : (s.expires_at ? new Date(s.expires_at).toLocaleString() : '-');

      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid var(--border-color);max-width:200px;overflow:hidden;text-overflow:ellipsis;" title="' + escHtml(s.filename) + '">' + escHtml(s.filename) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);color:' + statusColor + ';">' + statusText + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + downloads + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;">' + expiresLabel + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">';
      if (!s.expired) {
        html += '<button class="btn btn-danger btn-sm share-revoke-btn" data-token="' + escHtml(s.token) + '">撤销</button>';
      }
      html += '<button class="btn btn-sm btn-secondary share-copy-btn" data-token="' + escHtml(s.token) + '" style="margin-left:4px;">复制</button>';
      html += '</td></tr>';
    }
    html += '</tbody></table>';
    body.innerHTML = html;
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  }
}

async function revokeShare(token) {
  if (!confirm('确定撤销此分享链接？撤销后链接将立即失效。')) return;
  try {
    await sc.share.revoke(token);
    showToast('分享链接已撤销', 'success');
    refreshShareList();
  } catch (e) { showToast('撤销失败: ' + e.message, 'error'); }
}

function copyShareLink(token) {
  var url = location.origin + '/s/' + token;
  if (navigator.clipboard) {
    navigator.clipboard.writeText(url).then(function() {
      showToast('链接已复制到剪贴板', 'success');
    }).catch(function() {
      showToast('复制失败', 'error');
    });
  } else {
    showToast(url, 'success');
  }
}

// --- 云端下载 ---
let _cloudTasks = [];

// genDefaultFilename 从 URL 推断默认文件名，委托给共享模块 cloudfilename.js。
// 与 Go 端 pkg/cloudfilename 使用同一套规则（wget 行为），由共享语料测试保证双端一致。
function genDefaultFilename(rawUrl) {
  return cloudfilename.genDefaultFilename(rawUrl);
}

// filepathSafe 清理文件名中的路径分隔符，委托给共享模块，与 Go 端 pkg/cloudfilename.Safe 一致。
function filepathSafe(name) {
  return cloudfilename.filepathSafe(name);
}

// showCloudDownloadPreview 在提交前展示每个 URL 的默认文件名，供用户确认或修改。
// 支持每行 "URL" 或 "URL<TAB>FILENAME"（Tab 分隔的可选保存文件名，与 CLI --url-file
// 格式对齐；URL 本身可能含空格，文件名与 URL 之间必须用 Tab 分隔）。
// 含 Tab 的行把 FILENAME 预填入编辑框，用户仍可修改；确认时对所有 filename 走
// filepathSafe 保证最终保存名可靠。
async function showCloudDownloadPreview(action) {
  const input = document.getElementById('cloud-url');
  const text = input.value.trim();
  if (!text) { showToast('请输入下载链接', 'warning'); return; }

  const lines = text.split('\n').map(function(l) { return l.trim(); }).filter(function(l) { return l.length > 0; });
  if (lines.length === 0) { showToast('请输入下载链接', 'warning'); return; }

  if ((action === 'group' || action === 'chain_group') && lines.length < 2) {
    showToast('创建组至少需要 2 个 URL（单 URL 请用"仅提交"）', 'warning');
    return;
  }

  // 解析每行：含 Tab 时按前两列拆分出 URL 与预填文件名（多余 Tab 忽略，与 CLI readEntriesFromFile 一致）
  var parsedLines = [];
  for (var i = 0; i < lines.length; i++) {
    var parts = lines[i].split('\t');
    var url = parts[0].trim();
    var presetFilename = parts.length > 1 ? parts[1].trim() : '';
    parsedLines.push({ url: url, preset: presetFilename });
  }

  // 生成预览信息
  var previewHtml = '<div style="margin-bottom:12px;font-size:13px;color:var(--text-secondary);">共 ' + parsedLines.length + ' 个链接，请确认或修改保存文件名：</div>';
  previewHtml += '<div style="max-height:300px;overflow-y:auto;margin-bottom:12px;">';

  for (var i = 0; i < parsedLines.length; i++) {
    // 与服务端保存规则一致：展示清理后的最终文件名，避免"预览 a/b 实际保存 a_b"的落差
    // 显式指定的文件名也先 safe 化，保证预览即最终保存名
    var defaultName = parsedLines[i].preset
      ? cloudfilename.filepathSafe(parsedLines[i].preset)
      : cloudfilename.safeDefaultFromURL(parsedLines[i].url);
    previewHtml += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px;padding:4px 0;border-bottom:1px solid var(--border-color);">';
    previewHtml += '<span style="flex-shrink:0;font-size:12px;color:var(--text-muted);min-width:28px;">' + (i + 1) + '.</span>';
    previewHtml += '<input type="text" class="cloud-preview-filename" data-index="' + i + '" value="' + escHtml(defaultName) + '" style="flex:1;padding:4px 6px;border:1px solid var(--border-input);border-radius:3px;font-size:13px;font-family:monospace;">';
    previewHtml += '<span style="font-size:11px;color:var(--text-muted);max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(parsedLines[i].url) + '">' + escHtml(parsedLines[i].url) + '</span>';
    previewHtml += '</div>';
  }
  previewHtml += '</div>';

  previewHtml += '<div style="display:flex;gap:8px;justify-content:flex-end;">';
  previewHtml += '<button type="button" id="cloud-preview-cancel-btn" class="btn btn-secondary">取消</button>';
  previewHtml += '<button type="button" id="cloud-preview-confirm-btn" class="btn btn-primary">确认提交</button>';
  previewHtml += '</div>';

  // 保存 action 与解析后的行数据，用于确认后执行
  window._cloudPreviewAction = action;
  window._cloudPreviewLines = parsedLines;

  // 替换 cloud-url 区域为预览界面
  var urlRow = input.parentElement;
  urlRow.innerHTML = previewHtml;

  // 绑定按钮事件
  document.getElementById('cloud-preview-cancel-btn').addEventListener('click', function() {
    // 恢复输入框（含重新绑定四个按钮事件）
    restoreCloudUrlRow();
    document.getElementById('cloud-url').value = text;
  });

  document.getElementById('cloud-preview-confirm-btn').addEventListener('click', function() {
    // 收集用户指定的文件名
    var filenameInputs = document.querySelectorAll('.cloud-preview-filename');
    var urls = [];
    var filenames = [];
    for (var j = 0; j < filenameInputs.length; j++) {
      var url = window._cloudPreviewLines[j].url;
      urls.push(url);
      // 与服务端一致做 filepathSafe：用户输入的非法字符也会被清理，预览即最终保存名
      var name = filenameInputs[j].value.trim() || cloudfilename.safeDefaultFromURL(url);
      filenames.push(filepathSafe(name));
    }

    // 执行对应操作
    var act = window._cloudPreviewAction;
    // 提交前做 URL 格式预校验（对齐 Go cloudfilename.ValidateEntry，提供"服务端别拒绝"的体验）：
    // 无效 URL 立即提示，避免发送后拿到 400/409 往返。组内重复 URL 由下方文件名冲突检测
    // 与服务端 409 兜底（validateEntries 的去重判断与文件名冲突检测冗余，此处不重复调用）。
    var invalidURLs = [];
    for (var u = 0; u < urls.length; u++) {
      if (!cloudfilename.validateEntry(urls[u]).valid) {
        invalidURLs.push(urls[u]);
      }
    }
    if (invalidURLs.length > 0) {
      showToast('以下链接无效（需以 http/https 开头且含主机名）: '
        + invalidURLs.slice(0, 3).join(', ') + (invalidURLs.length > 3 ? ' 等 ' + invalidURLs.length + ' 个' : ''), 'warning');
      return;
    }
    if (act === 'group' || act === 'chain_group') {
      // 客户端预校验：组内保存文件名必须唯一（服务端 CreateGroup 也会校验并返回 409），
      // 这里在发送前拦截，避免 409 往返，直接提示用户修改冲突条目。
      // 用 Map 判重，避免普通对象把 constructor/toString 等原型属性误判为冲突。
      var seenNames = new Map();
      var conflicts = [];
      for (var k = 0; k < filenames.length; k++) {
        if (seenNames.has(filenames[k])) {
          conflicts.push(filenames[k]);
        } else {
          seenNames.set(filenames[k], k);
        }
      }
      if (conflicts.length > 0) {
        showToast('组内文件名冲突: ' + conflicts.join(', ') + '，请修改保存文件名', 'error');
        return;
      }
    }
    // 提交前立即恢复输入行，使"确认提交"按钮消失，防止链式等待（最长 20 分钟）期间
    // 用户重复点击同一批 URL；doXxx 使用已收集的 lines/filenames 局部变量，不受影响。
    restoreCloudUrlRow();
    var restoredInput = document.getElementById('cloud-url');
    if (restoredInput) restoredInput.value = '';
    if (act === 'submit') {
      doSubmitCloudTasks(urls, filenames);
    } else if (act === 'group') {
      doCreateCloudGroup(urls, filenames);
    } else if (act === 'chain') {
      doChainDownloadCloud(urls, filenames);
    } else if (act === 'chain_group') {
      doChainDownloadCloudGroup(urls, filenames);
    }
  });
}
let _cloudGroups = [];
let _cloudPollTimer = null;

// bindCloudUrlRowEvents 为云端下载输入行的按钮与 Enter 快捷键绑定事件。
// 必须在 restoreCloudUrlRow 重建输入行后也调用：重建的 textarea 是全新元素，
// 若不在此处补绑 keydown，Enter 只插入换行而不提交，与首次打开行为不一致。
function bindCloudUrlRowEvents() {
  document.getElementById('cloud-chain-btn').addEventListener('click', chainDownloadCloud);
  document.getElementById('cloud-submit-btn').addEventListener('click', createCloudTask);
  document.getElementById('cloud-create-group-btn').addEventListener('click', createCloudGroup);
  var chainGroupBtn = document.getElementById('cloud-chain-group-btn');
  if (chainGroupBtn) chainGroupBtn.addEventListener('click', chainDownloadCloudGroup);
  var urlInput = document.getElementById('cloud-url');
  if (urlInput) {
    urlInput.addEventListener('keydown', function(e) {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); createCloudTask(); }
    });
  }
}

// restoreCloudUrlRow 恢复云端下载输入行。预览界面（showCloudDownloadPreview）把
// #cloud-url 所在行的 innerHTML 替换为预览列表；若用户不点预览的"取消"而直接关闭
// 弹窗，输入行不会自动恢复。再次打开弹窗时 showCloudDownload 依赖 #cloud-url 存在，
// 缺失会导致 .value 抛 TypeError、弹窗空白。这里重建输入行并重新绑定按钮事件。
function restoreCloudUrlRow() {
  var urlRow = document.querySelector('#cloud-modal .cloud-url-row');
  if (!urlRow || document.getElementById('cloud-url')) return;
  // 静态可信模板，无用户输入拼接；用户内容通过 .value 赋值，不进入 innerHTML
  urlRow.innerHTML = '<textarea id="cloud-url" placeholder="输入下载链接，每行一个..." aria-label="下载链接" rows="3" style="flex:1;padding:8px;border:1px solid var(--border-input);border-radius:4px;font-size:14px;resize:vertical;font-family:inherit;"></textarea>' +
    '<button type="button" id="cloud-chain-btn" class="btn btn-primary" style="white-space:nowrap;">链式下载</button>' +
    '<button type="button" id="cloud-submit-btn" class="btn btn-secondary" style="white-space:nowrap;">仅提交</button>' +
    '<button type="button" id="cloud-create-group-btn" class="btn btn-secondary" style="white-space:nowrap;">创建组</button>' +
    '<button type="button" id="cloud-chain-group-btn" class="btn btn-primary" style="white-space:nowrap;">组链式下载</button>';
  bindCloudUrlRowEvents();
}

function showCloudDownload() {
  document.getElementById('cloud-modal').style.display = 'flex';
  // 先恢复可能的预览残留，确保 #cloud-url 存在（否则 .value 抛 TypeError）
  restoreCloudUrlRow();
  document.getElementById('cloud-url').value = '';
  refreshCloudTasks();
  refreshCloudGroups();
  startCloudPolling();
  switchCloudTab('tasks');
}

function hideCloudDownload() {
  document.getElementById('cloud-modal').style.display = 'none';
  stopCloudPolling();
}

let _cloudActiveTab = 'tasks';

function switchCloudTab(tab) {
  _cloudActiveTab = tab;
  document.getElementById('cloud-tasks-body').style.display = tab === 'tasks' ? 'block' : 'none';
  document.getElementById('cloud-groups-body').style.display = tab === 'groups' ? 'block' : 'none';
  document.querySelectorAll('.cloud-tab').forEach(function(el) {
    el.style.borderBottomColor = el.id === 'cloud-' + tab + '-tab' ? 'var(--tab-active)' : 'transparent';
    el.style.color = el.id === 'cloud-' + tab + '-tab' ? 'var(--text-primary)' : 'var(--text-secondary)';
  });
  if (tab === 'groups') refreshCloudGroups();
  if (tab === 'tasks') refreshCloudTasks();
}

// 刷新当前激活 Tab 的内容（刷新按钮按激活 Tab 分发）
function refreshCloudCurrentTab() {
  if (_cloudActiveTab === 'groups') refreshCloudGroups();
  else refreshCloudTasks();
}

function startCloudPolling() {
  stopCloudPolling();
  // 任务与组列表一起轮询，保证组进度同步刷新
  _cloudPollTimer = setInterval(function() {
    refreshCloudTasks();
    refreshCloudGroups();
  }, 3000);
}

function stopCloudPolling() {
  if (_cloudPollTimer) { clearInterval(_cloudPollTimer); _cloudPollTimer = null; }
}

let _cloudTasksInFlight = false;

async function refreshCloudTasks() {
  // 防重入：3s 轮询与手动刷新可能重叠，慢网络下后到的旧响应会覆盖新数据
  if (_cloudTasksInFlight) return;
  _cloudTasksInFlight = true;
  const body = document.getElementById('cloud-tasks-body');
  try {
    const data = await sc.cloud.listTasks({});
    const tasks = Array.isArray(data) ? data : (data && data.tasks) || [];
    _cloudTasks = tasks || [];
    if (_cloudTasks.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无下载任务</div>';
      return;
    }
    body.innerHTML = buildCloudTaskTableHtml(_cloudTasks);
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  } finally {
    _cloudTasksInFlight = false;
  }
}

// triggerBrowserDownload 触发浏览器保存（下载型归档等）。
function triggerBrowserDownload(data, filename) {
  const blob = new Blob([data], { type: 'application/octet-stream' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

async function chainDownloadCloud() {
  showCloudDownloadPreview('chain');
}

async function doChainDownloadCloud(lines, filenames) {
  // 防重入：链式等待期间不允许再次启动（模态关闭后后台继续跑，重复点击会并发两轮）
  if (window._busyChain) { showToast('已有链式下载在进行中', 'error'); return; }
  window._busyChain = true;
  try {
    const urls = lines.map(function(url, idx) { return { url: url, filename: filenames[idx] }; });
    showToast('提交任务中...', 'info');
    const tasks = await sc.cloud.createBatch(urls);
    refreshCloudTasks();
    showToast((tasks && tasks.tasks ? tasks.tasks.length : 0) + ' 个任务已提交', 'success');
    showToast('等待任务完成...', 'info');
    let taskList = (tasks && tasks.tasks) || [];
    for (let i = 0; i < 600; i++) {
      await new Promise(function(r) { setTimeout(r, 2000); });
      refreshCloudTasks();
      let allDone = true;
      for (let j = 0; j < taskList.length; j++) {
        try {
          const t = await sc.cloud.getTask(taskList[j].id);
          taskList[j] = t;
          if (t.status === 'pending' || t.status === 'downloading') { allDone = false; }
        } catch(e) {
          allDone = false;
        }
      }
      if (allDone) { break; }
    }
    const succeeded = taskList.filter(function(t) { return t.status === 'completed'; });
    const failedCount = taskList.filter(function(t) { return t.status === 'failed' || t.status === 'cancelled'; }).length;
    if (succeeded.length === 0) { showToast('所有任务均未成功完成', 'error'); return; }
    // 部分失败不再静默：提示用户只打包已完成的（禁止静默失败，I4）
    if (failedCount > 0) {
      showToast(failedCount + ' 个任务失败/取消，仅打包 ' + succeeded.length + ' 个已完成', 'warning');
    }
    showToast('打包归档中...', 'info');
    const taskIds = succeeded.map(function(t) { return t.id; });
    const archiveResult = await sc.cloud.archiveBatch(taskIds);
    if (!archiveResult.success) { showToast('归档失败: ' + (archiveResult.error || archiveResult.message || '未知错误'), 'error'); return; }
    showToast('下载归档并清理中...', 'info');
    const downloadName = (archiveResult.file || '').split('/').pop();
    // 先下载一次归档（I5），再逐个清理任务。
    const dlBlob = await sc.files.download(archiveResult.file);
    triggerBrowserDownload(dlBlob, downloadName);
    for (let i = 0; i < taskIds.length; i++) {
      try {
        await sc.cloud.deleteTask(taskIds[i]);
      } catch (e) {
        showToast('清理任务 ' + taskIds[i] + ' 失败: ' + e.message, 'error');
      }
    }
    refreshCloudTasks();
    if (failedCount > 0) {
      showToast('链式下载完成（' + succeeded.length + ' 成功, ' + failedCount + ' 失败/取消）', 'success');
    } else {
      showToast('链式下载完成!', 'success');
    }
  } catch (e) {
    showToast('链式下载失败: ' + e.message, 'error');
  } finally {
    window._busyChain = false;
  }
}

// 组链式下载入口：复用预览界面，action 为 'chain_group'
async function chainDownloadCloudGroup() {
  showCloudDownloadPreview('chain_group');
}

// doChainDownloadCloudGroup 执行组链式下载完整流程：创建组→等待→组级打包→下载→删除组。
// 与 batch chain 的语义一致：任一子任务 failed/cancelled → 整体失败。
async function doChainDownloadCloudGroup(urls, filenames) {
  // 防重入：链式等待期间不允许再次启动
  if (window._busyChain) { showToast('已有链式下载在进行中', 'error'); return; }
  window._busyChain = true;
  // 先 prompt 组名（与现有"创建组"行为一致）
  const name = prompt('组名称（可选）:', 'group-' + new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-'));
  if (name === null) { window._busyChain = false; return; }

  // groupId 提升到函数作用域：catch 块（独立块作用域）也需访问以清理已创建的组
  let groupId;
  try {
    showToast('创建下载组...', 'info');

    // 阶段 1: 创建组
    const entries = urls.map(function(url, idx) { return { url: url, filename: filenames[idx] }; });
    const groupData = await sc.cloud.createGroup(name, entries);
    groupId = groupData.id;
    let totalTasks = groupData.total_tasks || urls.length;
    refreshCloudGroups();
    showToast('下载组已创建: ' + groupId, 'success');

    // 阶段 2: 等待组内全部任务完成（轮询组详情，与 CLI CloudDownloadGroupChain.waitForGroup 逻辑对齐）
    showToast('等待 ' + totalTasks + ' 个任务完成...', 'info');
    let allDone = false;
    for (let i = 0; i < 600; i++) {
      await new Promise(function(r) { setTimeout(r, 2000); });
      refreshCloudGroups().catch(function() { /* 轮询失败忽略，主轮询继续 */ });
      try {
        const detail = await sc.cloud.getGroup(groupId);
        const group = detail.group || detail;
        // 检查是否有失败/取消的子任务
        let failed = 0, cancelled = 0, completed = 0, active = 0;
        const tasks = detail.tasks || [];
        for (let j = 0; j < tasks.length; j++) {
          switch (tasks[j].status) {
            case 'completed': completed++; break;
            case 'failed': failed++; break;
            case 'cancelled': cancelled++; break;
            default: active++;
          }
        }
        if (failed + cancelled > 0) {
          showToast('组内 ' + (failed + cancelled) + ' 个任务失败/取消，无法完成链式下载', 'error');
          // 保留组供 resume（与 CLI 链语义一致，不因失败丢弃已下载文件）
          return;
        }
        if (group.status === 'completed' || (active === 0 && completed > 0)) {
          allDone = true;
          break;
        }
      } catch(e) {
        // 轮询失败继续尝试
      }
    }
    if (!allDone) {
      showToast('等待超时', 'error');
      // 保留组供 resume（与 CLI 链语义一致）
      return;
    }
    showToast('所有任务已完成', 'success');

    // 阶段 3: 组级打包
    showToast('打包归档中...', 'info');
    const archiveName = 'group-' + groupId + '-' + Date.now() + '.tar.gz';
    const archiveResult = await sc.cloud.archiveGroup(groupId, archiveName);
    if (!archiveResult.success) {
      showToast('归档失败: ' + (archiveResult.error || archiveResult.message || '未知错误'), 'error');
      // 保留组供 resume（与 CLI 链语义一致）
      return;
    }
    showToast('下载归档并清理中...', 'info');

    // 阶段 4: 下载归档文件（先下载再删除组）
    const dlBlob = await sc.files.download(archiveResult.file);
    triggerBrowserDownload(dlBlob, archiveName);
    // 阶段 5: 删除组（清理远端文件）
    await deleteCloudGroupForCleanup(groupId);
    refreshCloudGroups();
    showToast('组链式下载完成!', 'success');
  } catch (e) {
    showToast('组链式下载失败: ' + e.message, 'error');
    // 失败保留组供 resume（与 CLI 链语义一致，不因异常丢弃已下载文件）
  } finally {
    window._busyChain = false;
  }
}

// deleteCloudGroupForCleanup 删除已创建的组（链式失败/超时/完成后的清理）。
// 组可能尚未创建（groupId 未定义）或已删除（幂等容忍 404）。
async function deleteCloudGroupForCleanup(groupId) {
  if (!groupId) return;
  try {
    await sc.cloud.deleteGroup(groupId);
  } catch (e) {
    showToast('清理下载组 ' + groupId + ' 失败: ' + e.message, 'error');
  }
}
async function createCloudTask() {
  showCloudDownloadPreview('submit');
}

async function doSubmitCloudTasks(lines, filenames) {
  try {
    if (lines.length === 1) {
      // 单 URL：使用原有 API，携带 filename
      const task = await sc.cloud.createDownload(lines[0], filenames[0]);
      showToast('任务已创建: ' + task.id, 'success');
    } else {
      // 多 URL：使用批量 API，携带每个 URL 的 filename
      const urls = lines.map(function(url, idx) { return { url: url, filename: filenames[idx] }; });
      const data = await sc.cloud.createBatch(urls);
      const tasks = (data && data.tasks) || [];
      const failed = tasks.filter(function(t) { return t.status === 'failed'; });
      const succeeded = tasks.filter(function(t) { return t.status !== 'failed'; });
      if (failed.length > 0) {
        showToast(succeeded.length + ' 个任务已创建, ' + failed.length + ' 个失败', 'warning');
      } else {
        showToast(tasks.length + ' 个任务已创建', 'success');
      }
    }
    refreshCloudTasks();
  } catch (e) { showToast('创建失败: ' + e.message, 'error'); }
}

// 创建云端下载任务组：把输入框中的所有 URL 作为一个组提交
async function createCloudGroup() {
  showCloudDownloadPreview('group');
}

async function doCreateCloudGroup(lines, filenames) {
  const name = prompt('组名称（可选）:', 'group-' + new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-'));
  if (name === null) return;

  try {
    const urls = lines.map(function(url, idx) { return { url: url, filename: filenames[idx] }; });
    await sc.cloud.createGroup(name, urls);
    showToast('下载组已创建', 'success');
    switchCloudTab('groups');
    refreshCloudGroups();
  } catch (e) { showToast('创建组失败: ' + e.message, 'error'); }
}

async function downloadCloudFile(taskId, filename, checksum) {
  try {
    // 先下载云端文件（sc.files.download 返回 Blob，传输层自动协商）
    const cloudPath = '.__cloud__/' + taskId + '/' + filename;
    const blob = await sc.files.download(cloudPath);

    // 触发浏览器下载
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    a.click();
    URL.revokeObjectURL(a.href);
    showToast('下载完成: ' + filename, 'success');

    // 清理云端副本
    await deleteCloudTask(taskId, filename, checksum);
  } catch (e) { showToast('下载失败: ' + e.message, 'error'); }
}

async function deleteCloudTask(taskId, filename, checksum) {
  try {
    // 删除云端文件（sc.files.deleteFile 走 checksum 校验）+ 删除任务
    const cloudPath = '.__cloud__/' + taskId + '/' + filename;
    await sc.files.deleteFile(cloudPath, checksum);
    await sc.cloud.deleteTask(taskId);
    refreshCloudTasks();
    return true;
  } catch (e) {
    // 不再静默吞错：清理失败提示用户（禁止静默失败），返回 false 供调用方决定
    showToast('清理云端副本失败: ' + e.message, 'error');
    refreshCloudTasks();
    return false;
  }
}

async function cancelCloudTask(taskId) {
  try {
    await sc.cloud.cancelTask(taskId);
    showToast('任务已取消', 'success');
    refreshCloudTasks();
  } catch (e) { showToast('取消失败: ' + e.message, 'error'); }
}

async function removeCloudTask(taskId) {
  try {
    await sc.cloud.deleteTask(taskId);
    showToast('任务已删除', 'success');
    refreshCloudTasks();
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

function buildCloudTaskTableHtml(tasks) {
  let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">文件名</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">大小</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';
  for (const t of tasks) {
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

function buildProgressBar(downloaded, total) {
  const pct = total > 0 ? Math.min(100, Math.round(downloaded * 100 / total)) : 0;
  return '<div style="margin-top:4px;height:6px;background:var(--progress-bg);border-radius:3px;overflow:hidden;min-width:80px;">' +
    '<div style="height:100%;width:' + pct + '%;background:var(--progress-bar);border-radius:3px;transition:width 0.5s;"></div>' +
    '</div><div style="font-size:11px;color:var(--text-secondary);margin-top:1px;">' + formatSize(downloaded) + ' / ' + formatSize(total) + ' (' + pct + '%)</div>';
}

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

function cloudTaskActions(id, filename, status, checksum) {
  let actions = '';
  if (status === 'completed') {
    actions += '<button class="btn btn-primary btn-sm cloud-download-btn" data-id="' + escHtml(id) + '" data-filename="' + escHtml(filename) + '" data-checksum="' + escHtml(checksum || '') + '" style="margin-right:4px;">下载到本地</button>';
    actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
  } else if (status === 'failed' || status === 'cancelled') {
    // failed 与 cancelled 都可恢复（服务端 ResumeTask 支持 cancelled），
    // 恢复时保留 .partial 续传；需要重下时用户可先删除再新建。
    actions += '<button class="btn btn-sm btn-secondary cloud-resume-btn" data-id="' + escHtml(id) + '" style="margin-right:4px;">恢复</button>';
    actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">删除</button>';
  } else {
    actions += '<button class="btn btn-warning btn-sm cloud-cancel-btn" data-id="' + escHtml(id) + '">取消</button>';
  }
  return actions;
}

// --- 文件版本管理 ---
function showVersioning() {
  document.getElementById('version-modal').style.display = 'flex';
  document.getElementById('version-filename').value = '';
  document.getElementById('version-body').innerHTML = '<div class="empty-msg">输入文件名查看版本历史</div>';
}

function hideVersioning() {
  document.getElementById('version-modal').style.display = 'none';
}

// --- 云端下载组管理 ---
let _cloudGroupsInFlight = false;

async function refreshCloudGroups() {
  const body = document.getElementById('cloud-groups-body');
  if (body.style.display === 'none') return;
  // 防重入：与任务刷新同一策略，跳过重叠请求避免旧响应覆盖新数据
  if (_cloudGroupsInFlight) return;
  _cloudGroupsInFlight = true;
  try {
    const data = await sc.cloud.listGroups({});
    const groups = Array.isArray(data) ? data : (data && data.groups) || [];
    _cloudGroups = groups || [];
    if (_cloudGroups.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无下载组</div>';
      return;
    }
    body.innerHTML = buildCloudGroupTableHtml(_cloudGroups);
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  } finally {
    _cloudGroupsInFlight = false;
  }
}

function buildCloudGroupTableHtml(groups) {
  let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">名称</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">状态</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">进度</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);"></th></tr></thead><tbody>';
  for (const g of groups) {
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

// toggleGroupTasks 展开/收起组内子任务详情。
async function toggleGroupTasks(groupId, btn) {
  var detailRow = document.getElementById('group-detail-' + groupId);
  if (!detailRow) return;
  if (detailRow.style.display !== 'none') {
    detailRow.style.display = 'none';
    btn.textContent = '展开';
    return;
  }
  detailRow.style.display = 'table-row';
  btn.textContent = '收起';
  var container = detailRow.querySelector('.group-task-list');
  try {
    const detail = await sc.cloud.getGroup(groupId);
    var tasks = detail.tasks || [];
    if (tasks.length === 0) {
      container.innerHTML = '<span style="color:var(--text-muted);">暂无子任务数据</span>';
      return;
    }
    var html = '<table class="file-table" style="width:100%;"><thead><tr><th style="text-align:left;padding:4px 8px;">ID</th><th style="text-align:left;padding:4px 8px;">URL</th><th style="text-align:left;padding:4px 8px;">状态</th><th style="text-align:left;padding:4px 8px;">进度</th><th style="text-align:left;padding:4px 8px;">ETag</th></tr></thead><tbody>';
    for (var i = 0; i < tasks.length; i++) {
      var t = tasks[i];
      html += '<tr>' +
        '<td style="padding:4px 8px;max-width:120px;overflow:hidden;text-overflow:ellipsis;">' + escHtml(t.id || '') + '</td>' +
        '<td style="padding:4px 8px;max-width:200px;overflow:hidden;text-overflow:ellipsis;" title="' + escHtml(t.url || '') + '">' + escHtml(t.url || '') + '</td>' +
        '<td style="padding:4px 8px;">' + statusText(t.status) + '</td>' +
        '<td style="padding:4px 8px;">' + (t.total_size > 0 ? buildProgressBar(t.downloaded, t.total_size) : '-') + '</td>' +
        '<td style="padding:4px 8px;max-width:180px;overflow:hidden;text-overflow:ellipsis;font-family:monospace;font-size:11px;" title="' + escHtml(t.etag || '') + '">' + escHtml(t.etag || '-') + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    container.innerHTML = html;
  } catch (e) {
    container.innerHTML = '<span style="color:var(--text-danger);">加载失败</span>';
    // 失败时恢复按钮文本和行状态
    detailRow.style.display = 'none';
    btn.textContent = '展开';
  }
}

async function archiveCloudGroup(groupId) {
  const archiveName = prompt('归档文件名:', 'group-' + groupId + '.tar.gz');
  if (!archiveName) return;
  try {
    await sc.cloud.archiveGroup(groupId, archiveName);
    showToast('打包成功', 'success');
    refreshCloudGroups();
  } catch (e) { showToast('打包失败: ' + e.message, 'error'); }
}

async function resumeCloudGroup(groupId) {
  // 第一个确认框决定"是否恢复"：取消则完全不做任何操作。
  if (!confirm('确认恢复该组内所有失败/取消任务？\n（默认使用断点续传，已下载的部分不重复下载）')) return;
  // 第二个确认框仅决定是否"强制重下"：确定=强制，取消=续传恢复（仍会执行恢复）。
  const force = confirm('是否强制重新下载（不使用续传）？\n点「确定」= 强制重新下载\n点「取消」= 使用续传恢复');
  try {
    await sc.cloud.resumeGroup(groupId, force);
    showToast('恢复成功', 'success');
    refreshCloudGroups();
  } catch (e) { showToast('恢复失败: ' + e.message, 'error'); }
}

async function cancelCloudGroup(groupId) {
  if (!confirm('确认取消该组内所有任务？')) return;
  try {
    await sc.cloud.cancelGroup(groupId);
    showToast('已取消', 'success');
    refreshCloudGroups();
  } catch (e) { showToast('取消失败: ' + e.message, 'error'); }
}

async function deleteCloudGroup(groupId) {
  if (!confirm('确认删除该组及所有关联文件？')) return;
  try {
    await sc.cloud.deleteGroup(groupId);
    showToast('已删除', 'success');
    refreshCloudGroups();
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

// 恢复单个云端下载任务
async function resumeCloudTask(taskId) {
  // 与组恢复一致：第一个确认决定是否恢复，第二个仅决定是否强制重下（取消=续传恢复）。
  if (!confirm('确认恢复该任务？\n（默认使用断点续传，已下载的部分不重复下载）')) return;
  const force = confirm('是否强制重新下载（不使用续传）？\n点「确定」= 强制重新下载\n点「取消」= 使用续传恢复');
  try {
    await sc.cloud.resumeTask(taskId, force);
    showToast('任务已恢复', 'success');
    refreshCloudTasks();
  } catch (e) { showToast('恢复失败: ' + e.message, 'error'); }
}

async function loadVersions() {
  var filename = document.getElementById('version-filename').value.trim();
  if (!filename) { showToast('请输入文件名', 'warning'); return; }
  var body = document.getElementById('version-body');
  body.innerHTML = '<div class="empty-msg">加载中...</div>';
  try {
    var data = await sc.files.versions.list(filename);
    var versions = data.versions || [];
    if (versions.length === 0) { body.innerHTML = '<div class="empty-msg">该文件没有版本历史</div>'; return; }
    body.innerHTML = buildVersionTableHtml(versions, filename);
  } catch (e) { body.innerHTML = '<div class="empty-msg">加载失败: ' + e.message + '</div>'; }
}

function buildVersionTableHtml(versions, filename) {
  var html = '<div style="margin-bottom:8px;font-size:13px;color:var(--text-secondary);">文件: <strong>' + escHtml(filename) + '</strong>，共 ' + versions.length + ' 个版本</div>';
  html += '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">版本 ID</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">时间</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid var(--border-color);">大小</th>' +
    '<th style="text-align:right;padding:4px 8px;border-bottom:1px solid var(--border-color);">操作</th></tr></thead><tbody>';
  for (var i = 0; i < versions.length; i++) {
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

async function restoreVersion(filename, versionId) {
  if (!confirm('确认恢复版本 ' + versionId + ' ？\n当前文件将被备份为新版本。')) return;
  try {
    const data = await sc.files.versions.restore(filename, versionId);
    if (data.success) { showToast('版本恢复成功', 'success'); loadVersions(); refreshList(); }
    else { showToast('恢复失败: ' + (data.message || 'unknown'), 'error'); }
  } catch (e) { showToast('恢复失败: ' + e.message, 'error'); }
}

async function deleteVersion(filename, versionId) {
  if (!confirm('确认删除版本 ' + versionId + ' ？\n此操作不可恢复。')) return;
  try {
    const data = await sc.files.versions.delete(filename, versionId);
    if (data.success) { showToast('版本已删除', 'success'); loadVersions(); }
    else { showToast('删除失败: ' + (data.message || 'unknown'), 'error'); }
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

// --- DOMContentLoaded 初始化：用 addEventListener 绑定所有静态 HTML 元素 ---
document.addEventListener('DOMContentLoaded', function() {
  // 认证栏
  document.getElementById('save-access-btn').addEventListener('click', saveAccessKeys);
  // 「走隧道（调试）」checkbox：初值取自 localStorage，change 即时生效（无需保存）
  var transportCb = document.getElementById('use-tunnel-checkbox');
  if (transportCb) {
    transportCb.checked = cuTunnelOverride();
    transportCb.addEventListener('change', function() { toggleTransport(transportCb); });
  }

  // 文件输入
  document.getElementById('file-input').addEventListener('change', function() {
    uploadFiles(this.files);
  });

  // 工具栏
  document.getElementById('refresh-btn').addEventListener('click', refreshList);
  document.getElementById('search-input').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') searchFiles();
  });
  document.getElementById('search-btn').addEventListener('click', searchFiles);
  document.getElementById('clear-search-btn').addEventListener('click', clearSearch);
  document.getElementById('stats-btn').addEventListener('click', showStats);
  document.getElementById('cloud-btn').addEventListener('click', showCloudDownload);
  document.getElementById('version-btn').addEventListener('click', showVersioning);
  document.getElementById('theme-toggle-btn').addEventListener('click', toggleTheme);

  // 批量操作
  document.getElementById('batch-delete-btn').addEventListener('click', batchDelete);
  document.getElementById('batch-rename-btn').addEventListener('click', batchRename);
  document.getElementById('batch-archive-btn').addEventListener('click', batchDownloadArchive);
  document.getElementById('batch-clear-btn').addEventListener('click', clearSelection);

  // 目录操作
  document.getElementById('mkdir-btn').addEventListener('click', mkdirDir);

  // 监控弹窗
  document.getElementById('stats-close-btn').addEventListener('click', hideStats);
  document.getElementById('stats-refresh-btn').addEventListener('click', showStats);
  document.getElementById('stats-close-modal-btn').addEventListener('click', hideStats);
  // 监控弹窗标签页切换
  document.getElementById('stats-tab').addEventListener('click', function() { switchStatsTab('stats'); });
  document.getElementById('config-tab').addEventListener('click', function() { switchStatsTab('config'); });
  document.getElementById('hub-tab').addEventListener('click', function() { switchStatsTab('hub'); });

  // 云端下载弹窗（按钮 + Enter 快捷键统一走 bindCloudUrlRowEvents，避免重复绑定）
  document.getElementById('cloud-close-btn').addEventListener('click', hideCloudDownload);
  bindCloudUrlRowEvents();
  document.getElementById('cloud-refresh-btn').addEventListener('click', refreshCloudCurrentTab);
  document.getElementById('cloud-close-modal-btn').addEventListener('click', hideCloudDownload);
  document.getElementById('cloud-tasks-tab').addEventListener('click', function() { switchCloudTab('tasks'); });
  document.getElementById('cloud-groups-tab').addEventListener('click', function() { switchCloudTab('groups'); });

  // 版本管理弹窗
  document.getElementById('version-close-btn').addEventListener('click', hideVersioning);
  document.getElementById('version-filename').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') loadVersions();
  });
  document.getElementById('version-load-btn').addEventListener('click', loadVersions);
  document.getElementById('version-close-modal-btn').addEventListener('click', hideVersioning);

  // 分享弹窗事件绑定
  document.getElementById('share-close-btn').addEventListener('click', hideShareModal);
  document.getElementById('share-create-tab').addEventListener('click', function() { switchShareTab('create'); });
  document.getElementById('share-list-tab').addEventListener('click', function() { switchShareTab('list'); });
  document.getElementById('share-create-btn').addEventListener('click', createShare);
  document.getElementById('share-list-refresh-btn').addEventListener('click', refreshShareList);

  // 事件委托：动态内容
  initDynamicEventDelegation();

  // 拖拽上传初始化
  initDragAndDrop();
});

// --- 事件委托：动态生成的 HTML 内容 ---
function initDynamicEventDelegation() {
  // 文件列表内的动态内容
  const fileList = document.getElementById('file-list');
  if (fileList) {
    // 文件行中的操作按钮
    fileList.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;

      // 文件操作按钮
      if (btn.classList.contains('file-download-btn')) {
        downloadFile(btn.dataset.filename, btn.dataset.checksum);
        return;
      }
      if (btn.classList.contains('file-delete-btn')) {
        deleteFile(btn.dataset.filename, btn.dataset.checksum);
        return;
      }
      if (btn.classList.contains('file-rename-btn')) {
        renameFile(btn.dataset.filename, btn.dataset.checksum);
        return;
      }
      if (btn.classList.contains('file-share-btn')) {
        showShareModal(btn.dataset.filename);
        return;
      }
      if (btn.classList.contains('file-preview-btn')) {
        previewFile(btn.dataset.filename);
        return;
      }

      // 目录操作按钮（需要阻止冒泡到行点击事件）
      if (btn.classList.contains('dir-enter-btn')) {
        e.stopPropagation();
        navigateDir(btn.dataset.subdir);
        return;
      }
      if (btn.classList.contains('dir-archive-btn')) {
        e.stopPropagation();
        downloadDirArchive(btn.dataset.subdir);
        return;
      }
      if (btn.classList.contains('dir-delete-btn')) {
        e.stopPropagation();
        rmdirDir(btn.dataset.subdir);
        return;
      }

      // 加载更多按钮
      if (btn.closest('#load-more-container')) {
        loadMore();
        return;
      }
    });

    // 目录行点击（导航到目录）
    fileList.addEventListener('click', function(e) {
      const dirRow = e.target.closest('.dir-row');
      if (dirRow && !e.target.closest('button')) {
        navigateDir(dirRow.dataset.subdir);
      }
    });

    // 全选复选框
    fileList.addEventListener('change', function(e) {
      if (e.target.id === 'select-all-checkbox') {
        toggleSelectAll(e.target.checked);
      }
    });

    // 单个文件选择复选框
    fileList.addEventListener('change', function(e) {
      if (e.target.classList.contains('file-select')) {
        updateBatchToolbar();
      }
    });
  }

  // checksum 点击复制
  document.addEventListener('click', function(e) {
    const cell = e.target.closest('.checksum-cell');
    if (cell) {
      copyChecksum(cell.dataset.checksum);
    }
  });

  // 云端下载任务操作
  const cloudBody = document.getElementById('cloud-tasks-body');
  if (cloudBody) {
    cloudBody.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;
      if (btn.classList.contains('cloud-download-btn')) {
        downloadCloudFile(btn.dataset.id, btn.dataset.filename, btn.dataset.checksum);
        return;
      }
      if (btn.classList.contains('cloud-remove-btn')) {
        removeCloudTask(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('cloud-cancel-btn')) {
        cancelCloudTask(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('cloud-resume-btn')) {
        resumeCloudTask(btn.dataset.id);
        return;
      }
    });
  }

  // 云端下载组操作
  const cloudGroupsBody = document.getElementById('cloud-groups-body');
  if (cloudGroupsBody) {
    cloudGroupsBody.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;
      if (btn.classList.contains('group-archive-btn')) {
        archiveCloudGroup(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('group-resume-btn')) {
        resumeCloudGroup(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('group-cancel-btn')) {
        cancelCloudGroup(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('group-delete-btn')) {
        deleteCloudGroup(btn.dataset.id);
        return;
      }
      if (btn.classList.contains('group-toggle-btn')) {
        toggleGroupTasks(btn.dataset.id, btn);
        return;
      }
    });
  }

  // 版本管理操作
  const versionBody = document.getElementById('version-body');
  if (versionBody) {
    versionBody.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;
      if (btn.classList.contains('version-restore-btn')) {
        restoreVersion(btn.dataset.filename, btn.dataset.versionId);
        return;
      }
      if (btn.classList.contains('version-delete-btn')) {
        deleteVersion(btn.dataset.filename, btn.dataset.versionId);
        return;
      }
    });
  }

  // 分享列表操作（事件委托）
  const shareListBody = document.getElementById('share-list-body');
  if (shareListBody) {
    shareListBody.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;
      if (btn.classList.contains('share-revoke-btn')) {
        revokeShare(btn.getAttribute('data-token'));
        return;
      }
      if (btn.classList.contains('share-copy-btn')) {
        copyShareLink(btn.getAttribute('data-token'));
        return;
      }
    });
  }

  // 配置面板更新按钮（事件委托）
  const configPanel = document.getElementById('config-panel');
  if (configPanel) {
    configPanel.addEventListener('click', function(e) {
      if (e.target.id === 'cfg-update-log-level') {
        var val = document.getElementById('cfg-log-level').value;
        updateConfigField('log_level', val);
      } else if (e.target.id === 'cfg-update-log-format') {
        var val = document.getElementById('cfg-log-format').value;
        updateConfigField('log_format', val);
      } else if (e.target.id === 'cfg-update-rate-limit') {
        var req = document.getElementById('cfg-rate-limit').value;
        updateConfigField('rate_limit_requests', parseInt(req) || 0);
        var win = document.getElementById('cfg-rate-window').value;
        updateConfigField('rate_limit_window', win);
      } else if (e.target.id === 'cfg-update-storage') {
        var val = document.getElementById('cfg-max-storage').value;
        updateConfigField('max_storage_bytes', parseInt(val) || 0);
      }
    });
  }

  // Hub 面板事件委托（移除节点按钮）
  const hubPanel = document.getElementById('hub-panel');
  if (hubPanel) {
    hubPanel.addEventListener('click', function(e) {
      if (e.target.classList.contains('hub-remove-btn')) {
        removeHubNode(e.target.getAttribute('data-node-id'));
      }
    });
  }
}

// 面包屑事件委托
document.addEventListener('DOMContentLoaded', function() {
  const breadcrumb = document.getElementById('dir-breadcrumb');
  if (breadcrumb) {
    breadcrumb.addEventListener('click', function(e) {
      const link = e.target.closest('a');
      if (link) {
        e.preventDefault();
        navigateDir(link.dataset.subdir || '');
      }
    });
  }
});

// --- 拖拽上传 ---
var dragCounter = 0;

function initDragAndDrop() {
  var container = document.getElementById('app');

  container.addEventListener('dragenter', function(e) {
    e.preventDefault();
    e.stopPropagation();
    dragCounter++;
    if (dragCounter === 1) {
      container.style.outline = '3px dashed var(--btn-primary-bg, #2b6cb5)';
      container.style.outlineOffset = '-8px';
      container.style.backgroundColor = 'var(--bg-auth, #f0f4ff)';
    }
  });

  container.addEventListener('dragover', function(e) {
    e.preventDefault();
    e.stopPropagation();
  });

  container.addEventListener('dragleave', function(e) {
    e.preventDefault();
    e.stopPropagation();
    dragCounter--;
    if (dragCounter === 0) {
      container.style.outline = 'none';
      container.style.backgroundColor = '';
    }
  });

  container.addEventListener('drop', function(e) {
    e.preventDefault();
    e.stopPropagation();
    dragCounter = 0;
    container.style.outline = 'none';
    container.style.backgroundColor = '';

    var files = e.dataTransfer.files;
    if (files.length > 0) {
      handleDroppedFiles(files);
    }
  });
}

function handleDroppedFiles(files) {
  var fileInput = document.getElementById('file-input');

  var dataTransfer = new DataTransfer();
  for (var i = 0; i < files.length; i++) {
    dataTransfer.items.add(files[i]);
  }
  fileInput.files = dataTransfer.files;

  var event = new Event('change', { bubbles: true });
  fileInput.dispatchEvent(event);
}

// --- 文件预览 ---
function previewFile(filename) {
  var ext = filename.split('.').pop().toLowerCase();

  if (['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp', 'svg'].indexOf(ext) !== -1) {
    previewImage(filename);
  } else if (['txt', 'md', 'json', 'yaml', 'yml', 'xml', 'csv', 'log', 'sh', 'bat', 'go', 'js', 'py', 'css', 'html', 'conf', 'ini', 'cfg'].indexOf(ext) !== -1) {
    previewText(filename);
  } else {
    downloadFile(filename);
  }
}

function previewImage(filename) {
  var url = '/download?filename=' + encodeURIComponent(filename);
  var modal = document.createElement('div');
  modal.className = 'modal-overlay-img';
  modal.style.cssText = 'position:fixed;inset:0;z-index:2000;display:flex;align-items:center;justify-content:center;cursor:pointer;';

  var img = document.createElement('img');
  img.style.cssText = 'max-width:90vw;max-height:90vh;object-fit:contain;border-radius:4px;box-shadow:0 4px 24px rgba(0,0,0,.5);';
  img.src = url;
  img.alt = filename;

  modal.appendChild(img);
  modal.addEventListener('click', function() { if (document.body.contains(modal)) document.body.removeChild(modal); });
  document.body.appendChild(modal);
}

async function previewText(filename) {
  try {
    const blob = await sc.files.download(filename);
    let text = await blob.text();
    var lines = text.split('\n');
    if (lines.length > 100) {
      text = lines.slice(0, 100).join('\n') + '\n\n... (共 ' + lines.length + ' 行，仅显示前 100 行)';
    }
    showTextPreview(filename, text);
  } catch (e) { showToast('预览失败: ' + e.message, 'error'); }
}

function showTextPreview(filename, text) {
  var modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.style.cssText = 'position:fixed;inset:0;z-index:2000;display:flex;align-items:center;justify-content:center;';

  var content = document.createElement('div');
  content.style.cssText = 'background:var(--modal-bg,#fff);border-radius:8px;padding:16px;width:700px;max-width:92vw;max-height:80vh;display:flex;flex-direction:column;';

  var header = document.createElement('div');
  header.style.cssText = 'display:flex;align-items:center;justify-content:space-between;margin-bottom:12px;';
  header.innerHTML = '<span style="font-size:14px;font-weight:600;color:var(--text-primary,#333);">' + escHtml(filename) + '</span>' +
    '<button style="background:none;border:none;font-size:20px;cursor:pointer;color:var(--text-secondary,#888);line-height:1;">&times;</button>';
  header.querySelector('button').addEventListener('click', function() { if (document.body.contains(modal)) document.body.removeChild(modal); });

  var pre = document.createElement('pre');
  pre.style.cssText = 'margin:0;padding:12px;background:var(--bg-hover,#f8f9fa);border-radius:4px;font-size:13px;line-height:1.5;overflow:auto;white-space:pre-wrap;word-break:break-all;max-height:60vh;color:var(--text-primary,#333);';
  pre.textContent = text;

  content.appendChild(header);
  content.appendChild(pre);
  modal.appendChild(content);
  modal.addEventListener('click', function(e) { if (e.target === modal && document.body.contains(modal)) document.body.removeChild(modal); });
  document.body.appendChild(modal);
}

