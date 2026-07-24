// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 主逻辑：文件列表、CRUD、批量操作、导航、UI 工具。
// 依赖 sha256.js, tunnel.js, upload.js（先加载）。

const BASE = '';
let token = localStorage.getItem('sproxy_token') || '';
let currentSubdir = localStorage.getItem('sproxy_subdir') || '';
let _searchActive = false;
let _currentOffset = 0;
let _hasMore = false;
const PAGE_LIMIT = 500;

document.getElementById('token').value = token;
document.getElementById('tunnel-key').value = tunnelHexKey || '';

function saveToken() {
  token = document.getElementById('token').value;
  localStorage.setItem('sproxy_token', token);
  showToast('Token 已保存', 'success');
}

function saveTunnelKey() {
  tunnelHexKey = document.getElementById('tunnel-key').value;
  localStorage.setItem('sproxy_tunnel_key', tunnelHexKey);
  _tunnelCryptoKey = null;
  showToast('Tunnel Key 已保存', 'success');
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

function escJsStr(s) {
  return String(s).replaceAll('\\', '\\\\').replaceAll("'", "\\'").replaceAll('"', '\\"');
}

function headers(extra) {
  const h = extra || {};
  if (token && !tunnelHexKey) h['Authorization'] = 'Bearer ' + token;
  return h;
}

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
    let files;
    let data;
    const qs = (currentSubdir ? '?subdir=' + encodeURIComponent(currentSubdir) + '&' : '?') + 'offset=0&limit=' + PAGE_LIMIT;
    const listUrl = '/api/files' + qs;
    if (tunnelHexKey) {
      const result = await tunnelRequest('GET', listUrl, {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
      files = data.files || [];
    } else {
      const resp = await fetch(BASE + listUrl, { headers: headers() });
      data = await resp.json();
      if (!resp.ok) { el.innerHTML = '<div class="empty-msg">加载失败: ' + escHtml(data.message || String(resp.status)) + '</div>'; return; }
      files = data.files || [];
    }
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
    let files;
    let data;
    if (tunnelHexKey) {
      const result = await tunnelRequest('GET', listUrl, {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
      files = data.files || [];
    } else {
      const resp = await fetch(BASE + listUrl, { headers: headers() });
      data = await resp.json();
      if (!resp.ok) return;
      files = data.files || [];
    }
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
        container.innerHTML = '<div style="text-align:center;padding:12px;color:#999;">已加载全部 ' + data.total + ' 个文件</div>';
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
    return '<tr style="cursor:pointer;background:#f8f9fa;" class="dir-row" data-subdir="' + escHtml(fullName) + '"><td class="check-col"></td><td><strong>' + escHtml(fi.name) + '/</strong></td>' +
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
    let files;
    const searchUrl = '/api/files/search?q=' + encodeURIComponent(q);
    if (tunnelHexKey) {
      const result = await tunnelRequest('GET', searchUrl, {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      files = data.files || [];
    } else {
      const resp = await fetch(BASE + searchUrl, { headers: headers() });
      if (!resp.ok) {
        const errData = await resp.json().catch(function() { return {}; });
        el.innerHTML = '<div class="empty-msg">搜索失败: ' + (errData.message || resp.status) + '</div>';
        return;
      }
      const data = await resp.json();
      files = data.files || [];
    }
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
    html += ' <span style="color:#999">›</span> <a href="#" data-subdir="' + escHtml(accumulated) + '">' + escHtml(p) + '</a>';
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
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/mkdir?dirname=' + encodeURIComponent(dirPath), {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) {
        showToast('目录已创建: ' + dirPath, 'success');
        input.value = '';
        refreshList();
      } else { showToast('创建目录失败: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/mkdir?dirname=' + encodeURIComponent(dirPath), { method: 'POST', headers: headers() });
      const data = await resp.json();
      if (resp.ok && data.success) {
        showToast('目录已创建: ' + dirPath, 'success');
        input.value = '';
        refreshList();
      } else { showToast('创建目录失败: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('创建目录失败: ' + e.message, 'error'); }
}

async function rmdirDir(dirPath) {
  if (!confirm('确认删除目录 "' + dirPath + '" 及其所有内容?')) return;
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/rmdir?dirname=' + encodeURIComponent(dirPath), {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('目录已删除: ' + dirPath, 'success'); refreshList(); }
      else { showToast('删除目录失败: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/rmdir?dirname=' + encodeURIComponent(dirPath), { method: 'POST', headers: headers() });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('目录已删除: ' + dirPath, 'success'); refreshList(); }
      else { showToast('删除目录失败: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('删除目录失败: ' + e.message, 'error'); }
}

// --- 下载 ---
async function downloadFile(name, expectedChecksum) {
  try {
    if (tunnelHexKey) {
      let result = await tunnelDownloadStream(name);
      if (!result) result = await tunnelRequest('GET', '/download?filename=' + encodeURIComponent(name), {}, null);
      const serverCS = (result.headers['X-File-Checksum'] || [''])[0];
      if (serverCS) {
        const sha256 = new Sha256();
        sha256.update(new Uint8Array(result.body));
        const localCS = sha256.digest();
        if (localCS !== serverCS) {
          showToast(name + ' 校验失败: 服务端 ' + serverCS.substring(0, 16) + '…, 本地 ' + localCS.substring(0, 16) + '…', 'error');
          return;
        }
      }
      triggerDownload(name, result.body);
      showToast(name + ' 下载完成' + (serverCS ? '，校验通过' : ''), 'success');
    } else {
      await directDownload(name);
    }
  } catch (e) { showToast('下载失败: ' + e.message, 'error'); }
}

async function directDownload(name) {
  const resp = await fetch(BASE + '/download?filename=' + encodeURIComponent(name), { headers: headers() });
  if (!resp.ok) {
    const data = await resp.json().catch(function() { return {}; });
    showToast('下载失败: ' + (data.message || resp.status), 'error');
    return;
  }
  const serverCS = resp.headers.get('X-File-Checksum') || '';
  const contentLength = Number.parseInt(resp.headers.get('Content-Length') || '0');

  if (serverCS) {
    const sha256 = new Sha256();
    if (contentLength > 100 * 1024 * 1024) {
      const reader = resp.body.getReader();
      let readResult = await reader.read();
      while (!readResult.done) {
        sha256.update(new Uint8Array(readResult.value));
        readResult = await reader.read();
      }
      const localCS = sha256.digest();
      if (localCS !== serverCS) {
        showToast(name + ' 校验失败: 服务端 ' + serverCS.substring(0, 16) + '…, 本地 ' + localCS.substring(0, 16) + '…', 'error');
        return;
      }
      const resp2 = await fetch(BASE + '/download?filename=' + encodeURIComponent(name), { headers: headers() });
      triggerDownload(name, await resp2.blob());
      showToast(name + ' 下载完成，校验通过', 'success');
      return;
    }
    const buffer = await resp.arrayBuffer();
    sha256.update(new Uint8Array(buffer));
    const localCS = sha256.digest();
    if (localCS !== serverCS) {
      showToast(name + ' 校验失败: 服务端 ' + serverCS.substring(0, 16) + '…, 本地 ' + localCS.substring(0, 16) + '…', 'error');
      return;
    }
    triggerDownload(name, buffer);
    showToast(name + ' 下载完成，校验通过', 'success');
    return;
  }
  triggerDownload(name, await resp.blob());
  showToast(name + ' 下载完成', 'success');
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
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/delete?filename=' + encodeURIComponent(name), { 'X-File-Checksum': checksum }, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('删除成功: ' + name, 'success'); refreshList(); }
      else { showToast('删除失败: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/delete?filename=' + encodeURIComponent(name), {
        method: 'POST', headers: headers({ 'X-File-Checksum': checksum })
      });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('删除成功: ' + name, 'success'); refreshList(); }
      else { showToast('删除失败: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

// --- 重命名 ---
async function renameFile(name, checksum) {
  if (!checksum) { showToast('缺少 checksum，无法校验完整性', 'error'); return; }
  const newName = prompt('新的文件名（路径）:', name);
  if (!newName || newName === name) return;
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/rename?from=' + encodeURIComponent(name) + '&to=' + encodeURIComponent(newName), { 'X-File-Checksum': checksum }, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('重命名成功: ' + newName, 'success'); refreshList(); }
      else { showToast('重命名失败: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/rename?from=' + encodeURIComponent(name) + '&to=' + encodeURIComponent(newName), {
        method: 'POST', headers: headers({ 'X-File-Checksum': checksum })
      });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('重命名成功: ' + newName, 'success'); refreshList(); }
      else { showToast('重命名失败: ' + (data.message || resp.status), 'error'); }
    }
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
  const body = JSON.stringify({ files: files });
  try {
    const data = await sendBatchRequest('/api/batch/delete', body);
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
    const data = await sendBatchRequest('/api/batch/rename', JSON.stringify({ operations: operations }));
    if (data.success) { showToast(data.message || '重命名完成', 'success'); clearSelection(); refreshList(); }
    else { showToast(data.message || '批量重命名失败', 'error'); }
  } catch (e) { showToast('批量重命名失败: ' + e.message, 'error'); }
}

async function sendBatchRequest(url, body) {
  if (tunnelHexKey) {
    const result = await tunnelRequest('POST', url, { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
    return JSON.parse(new TextDecoder().decode(result.body));
  }
  const resp = await fetch(BASE + url, { method: 'POST', headers: headers({ 'Content-Type': 'application/json' }), body: body });
  return resp.json();
}

function batchDownloadArchive() {
  const selected = getSelectedFiles();
  if (selected.length === 0) { showToast('请选择文件', 'warning'); return; }
  const files = selected.map(function(f) { return f.filename; });
  const headersObj = headers();
  headersObj['Content-Type'] = 'application/json';
  fetch(BASE + '/api/archive', {
    method: 'POST', headers: headersObj, body: JSON.stringify({ files: files })
  }).then(function(resp) {
    if (!resp.ok) return resp.text().then(function(t) { throw new Error(t); });
    const disposition = resp.headers.get('Content-Disposition') || '';
    const match = disposition.match(/filename="?(.+?)"?$/);
    const filename = match ? match[1] : 'archive.tar.gz';
    return resp.blob().then(function(blob) { triggerDownload(filename, blob); showToast('归档下载完成: ' + filename, 'success'); });
  }).catch(function(err) { showToast('归档失败: ' + err.message, 'error'); });
}

// 目录打包下载（GET /api/archive-dir）
async function downloadDirArchive(dirPath) {
  try {
    var url = '/api/archive-dir?dirname=' + encodeURIComponent(dirPath);
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', url, {}, null);
      triggerDownload(dirPath.replace('/', '_') + '.tar.gz', result.body);
      showToast('目录打包下载完成', 'success');
    } else {
      var resp = await fetch(BASE + url, { headers: headers() });
      if (!resp.ok) {
        var errData = await resp.json().catch(function() { return {}; });
        showToast('打包下载失败: ' + (errData.message || resp.status), 'error');
        return;
      }
      var disposition = resp.headers.get('Content-Disposition') || '';
      var match = disposition.match(/filename="?(.+?)"?$/);
      var filename = match ? match[1] : dirPath.replace('/', '_') + '.tar.gz';
      var blob = await resp.blob();
      triggerDownload(filename, blob);
      showToast('目录打包下载完成: ' + filename, 'success');
    }
  } catch (e) { showToast('打包下载失败: ' + e.message, 'error'); }
}

// --- 监控 ---
async function showStats() {
  document.getElementById('stats-modal').style.display = 'flex';
  switchStatsTab('stats');
  document.getElementById('stats-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">加载中...</div>';
  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/stats', {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
    } else {
      var resp = await fetch(BASE + '/api/stats', { headers: headers() });
      if (!resp.ok) { document.getElementById('stats-panel').innerHTML = '<div style="color:red">请求失败: ' + resp.status + '</div>'; return; }
      data = await resp.json();
    }
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
    el.style.borderBottomColor = el.id === tab + '-tab' ? '#4a90d9' : 'transparent';
    el.style.color = el.id === tab + '-tab' ? '#333' : '#666';
  });
  if (tab === 'config') showConfig();
  if (tab === 'hub') showHub();
}

async function showConfig() {
  document.getElementById('config-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">加载中...</div>';
  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/config', {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
    } else {
      var resp = await fetch(BASE + '/api/config', { headers: headers() });
      if (!resp.ok) { document.getElementById('config-panel').innerHTML = '<div style="color:red">请求失败: ' + resp.status + '</div>'; return; }
      data = await resp.json();
    }
    document.getElementById('config-panel').innerHTML = configTableHtml(data);
  } catch (e) { document.getElementById('config-panel').innerHTML = '<div style="color:red">错误: ' + e.message + '</div>'; }
}

// --- Hub 管理 ---
async function showHub() {
  document.getElementById('hub-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">加载中...</div>';
  try {
    var nodes, stats;
    if (tunnelHexKey) {
      var [nResult, sResult] = await Promise.all([
        tunnelRequest('GET', '/api/hub/nodes', {}, null),
        tunnelRequest('GET', '/api/hub/stats', {}, null)
      ]);
      nodes = JSON.parse(new TextDecoder().decode(nResult.body)) || [];
      stats = JSON.parse(new TextDecoder().decode(sResult.body));
    } else {
      var [nResp, sResp] = await Promise.all([
        fetch(BASE + '/api/hub/nodes', { headers: headers() }),
        fetch(BASE + '/api/hub/stats', { headers: headers() })
      ]);
      if (!nResp.ok) {
        document.getElementById('hub-panel').innerHTML = '<div class="empty-msg">Hub 未启用或请求失败</div>';
        return;
      }
      nodes = await nResp.json();
      stats = await sResp.json();
    }
    document.getElementById('hub-panel').innerHTML = hubTableHtml(nodes, stats);
  } catch (e) {
    document.getElementById('hub-panel').innerHTML = '<div class="empty-msg">Hub 未启用或请求失败: ' + e.message + '</div>';
  }
}

function hubTableHtml(nodes, stats) {
  var html = '';
  // 统计概要
  if (stats) {
    html += '<div style="margin-bottom:12px;padding:8px 12px;background:#f0f8ff;border-radius:4px;font-size:13px;">';
    html += '已连接节点: <strong>' + (stats.nodes_connected ?? 0) + '</strong></div>';
  }

  if (!nodes || nodes.length === 0) {
    html += '<div class="empty-msg">暂无已连接节点</div>';
    return html;
  }

  html += '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
  html += '<thead><tr style="background:#f5f5f5;">';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">节点 ID</th>';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">地址</th>';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">连接时间</th>';
  html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid #ddd;">操作</th>';
  html += '</tr></thead><tbody>';

  for (var i = 0; i < nodes.length; i++) {
    var n = nodes[i];
    var connected = n.connected ? new Date(n.connected).toLocaleString() : '-';
    html += '<tr>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-family:monospace;font-size:12px;">' + escHtml(n.id) + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;">' + escHtml(n.addr || '-') + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-size:12px;">' + connected + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;text-align:center;">';
    html += '<button class="btn btn-danger btn-sm hub-remove-btn" data-node-id="' + escHtml(n.id) + '">移除</button>';
    html += '</td></tr>';
  }
  html += '</tbody></table>';
  return html;
}

async function removeHubNode(nodeId) {
  if (!confirm('确定移除节点 ' + nodeId + '？')) return;
  try {
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', '/api/hub/nodes/' + encodeURIComponent(nodeId), {}, null);
    } else {
      var resp = await fetch(BASE + '/api/hub/nodes/' + encodeURIComponent(nodeId), { method: 'DELETE', headers: headers() });
      if (!resp.ok) {
        var data = await resp.json().catch(function() { return {}; });
        showToast('移除失败: ' + (data.error || resp.status), 'error');
        return;
      }
    }
    showToast('节点 ' + nodeId + ' 已移除', 'success');
    showHub();
  } catch (e) { showToast('移除失败: ' + e.message, 'error'); }
}

function configTableHtml(cfg) {
  var html = '<table style="width:100%;border-collapse:collapse;font-size:14px;">';
  html += '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555">运行时配置</th></tr>';

  function row(label, value) {
    return '<tr><td style="padding:5px 0;color:#777">' + label + '</td><td style="text-align:right">' + (value ?? '-') + '</td></tr>';
  }

  html += row('日志级别', cfg.log_level);
  html += row('日志格式', cfg.log_format);
  html += row('认证令牌', cfg.auth_token_set ? '✅ 已设置' : '❌ 未设置');
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
  html += '<div style="margin-top:16px;padding-top:12px;border-top:1px solid #eee;">';
  html += '<div style="font-size:13px;font-weight:600;color:#555;margin-bottom:8px;">快速编辑</div>';

  // 日志级别
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">日志级别:</span>';
  html += '<select id="cfg-log-level" style="padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  var levels = ['debug','info','warn','error'];
  for (var i = 0; i < levels.length; i++) {
    html += '<option value="' + levels[i] + '"' + (cfg.log_level === levels[i] ? ' selected' : '') + '>' + levels[i] + '</option>';
  }
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-level">更新</button></div>';

  // 日志格式
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">日志格式:</span>';
  html += '<select id="cfg-log-format" style="padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<option value="text"' + (cfg.log_format === 'text' ? ' selected' : '') + '>text</option>';
  html += '<option value="json"' + (cfg.log_format === 'json' ? ' selected' : '') + '>json</option>';
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-format">更新</button></div>';

  // 速率限制
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">速率限制:</span>';
  html += '<input type="number" id="cfg-rate-limit" value="' + (cfg.rate_limit_requests ?? 10) + '" style="width:60px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<span style="font-size:12px;color:#999;">req / </span>';
  html += '<input type="text" id="cfg-rate-window" value="' + (cfg.rate_limit_window || '1s') + '" style="width:60px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-rate-limit">更新</button></div>';

  // 存储限制
  html += '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">存储上限:</span>';
  html += '<input type="number" id="cfg-max-storage" value="' + (cfg.max_storage_bytes ?? 0) + '" style="width:140px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;" min="0">';
  html += '<span style="font-size:12px;color:#999;">字节（0=不限）</span>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-storage">更新</button></div>';

  html += '</div>';
  return html;
}

async function updateConfigField(key, value) {
  var body = JSON.stringify((function() { var o = {}; o[key] = value; return o; })());
  try {
    if (tunnelHexKey) {
      var result = await tunnelRequest('PUT', '/api/config', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('配置已更新', 'success'); showConfig(); }
      else { showToast('更新失败', 'error'); }
    } else {
      var resp = await fetch(BASE + '/api/config', {
        method: 'PUT', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('配置已更新', 'success'); showConfig(); }
      else { showToast('更新失败: ' + (data.error || resp.status), 'error'); }
    }
  } catch (e) { showToast('更新失败: ' + e.message, 'error'); }
}

// 旧版 updateStorageConfig，改用配置面板中的 cfg-update-storage 代替
async function updateStorageConfig() {
  var input = document.getElementById('max-storage-input');
  var maxBytes = Number.parseInt(input.value) || 0;
  if (maxBytes < 0) { showToast('存储限制不能为负数', 'error'); return; }
  try {
    var body = JSON.stringify({ max_storage_bytes: maxBytes });
    if (tunnelHexKey) {
      var result = await tunnelRequest('PUT', '/api/storage/config', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('存储限制已更新: ' + formatBytes(data.max_storage_bytes || 0), 'success'); }
      else { showToast('更新失败', 'error'); }
    } else {
      var resp = await fetch(BASE + '/api/storage/config', {
        method: 'PUT', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('存储限制已更新: ' + formatBytes(data.max_storage_bytes || 0), 'success'); }
      else { showToast('更新失败: ' + (data.error || resp.status), 'error'); }
    }
  } catch (e) { showToast('更新失败: ' + e.message, 'error'); }
}

function formatBytes(n) {
  if (n == null) return '-';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
  return (n / 1073741824).toFixed(2) + ' GB';
}

function statsTableHtml(du, rc, s) {
  return '<table style="width:100%;border-collapse:collapse;font-size:14px;">' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555">磁盘使用</th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">目录</td><td style="text-align:right">' + (du.uploads_dir || '-') + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">文件数</td><td style="text-align:right">' + (du.total_files ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">总大小</td><td style="text-align:right">' + formatBytes(du.total_size) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555;padding-top:14px">请求统计（自启动）</th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">总请求数</td><td style="text-align:right">' + (rc.total ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">2xx</td><td style="text-align:right">' + (rc['2xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">4xx</td><td style="text-align:right">' + (rc['4xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">5xx</td><td style="text-align:right">' + (rc['5xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">活跃连接</td><td style="text-align:right">' + (s.active_connections ?? 0) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555;padding-top:14px">传输统计（自启动）</th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">上传文件数</td><td style="text-align:right">' + (s.files_uploaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">上传字节数</td><td style="text-align:right">' + formatBytes(s.bytes_uploaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">下载文件数</td><td style="text-align:right">' + (s.files_downloaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">下载字节数</td><td style="text-align:right">' + formatBytes(s.bytes_downloaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">删除文件数</td><td style="text-align:right">' + (s.files_deleted ?? 0) + '</td></tr></table>';
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
  var body = JSON.stringify({
    filename: name,
    ttl: ttl,
    max_downloads: maxDownloads,
    one_time: oneTime
  });
  (async function() {
    try {
      var data;
      if (tunnelHexKey) {
        var result = await tunnelRequest('POST', '/api/share', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
        data = JSON.parse(new TextDecoder().decode(result.body));
      } else {
        var resp = await fetch(BASE + '/api/share', {
          method: 'POST', headers: headers({ 'Content-Type': 'application/json' }), body: body
        });
        data = await resp.json();
        if (!resp.ok) { showToast('创建分享失败: ' + (data.message || resp.status), 'error'); return; }
      }
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
    el.style.borderBottomColor = el.id === 'share-' + tab + '-tab' ? '#4a90d9' : 'transparent';
    el.style.color = el.id === 'share-' + tab + '-tab' ? '#333' : '#666';
  });
}

async function createShare() {
  var filename = document.getElementById('share-filename').value.trim();
  if (!filename) { showToast('请输入文件名', 'error'); return; }
  var ttl = document.getElementById('share-ttl').value.trim() || '24h';
  var maxDownloads = Number.parseInt(document.getElementById('share-max-downloads').value) || 0;
  var oneTime = document.getElementById('share-one-time').checked;

  var body = JSON.stringify({ filename: filename, ttl: ttl, max_downloads: maxDownloads, one_time: oneTime });

  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('POST', '/api/share', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.message && data.message !== 'ok') { showToast('创建分享失败: ' + data.message, 'error'); return; }
    } else {
      var resp = await fetch(BASE + '/api/share', {
        method: 'POST', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      data = await resp.json();
      if (!resp.ok) { showToast('创建分享失败: ' + (data.message || resp.status), 'error'); return; }
    }
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
    var shares;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/shares', {}, null);
      shares = (JSON.parse(new TextDecoder().decode(result.body))).shares || [];
    } else {
      var resp = await fetch(BASE + '/api/shares', { headers: headers() });
      if (!resp.ok) { body.innerHTML = '<div class="empty-msg">请求失败: ' + resp.status + '</div>'; return; }
      shares = (await resp.json()).shares || [];
    }

    if (shares.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无分享链接</div>';
      return;
    }

    var html = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:#f5f5f5;"><th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">文件名</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">状态</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">下载次数</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">过期时间</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid #ddd;">操作</th></tr></thead><tbody>';

    for (var i = 0; i < shares.length; i++) {
      var s = shares[i];
      var statusText = s.expired ? '已过期' : (s.one_time ? '一次性' : '活跃');
      var statusColor = s.expired ? '#999' : (s.one_time ? '#e67e22' : '#27ae60');
      var downloads = s.max_downloads > 0 ? s.downloads + '/' + s.max_downloads : s.downloads + '/∞';
      var expiresLabel = s.expired ? '-' : (s.expires_at ? new Date(s.expires_at).toLocaleString() : '-');

      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid #eee;max-width:200px;overflow:hidden;text-overflow:ellipsis;" title="' + escHtml(s.filename) + '">' + escHtml(s.filename) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;color:' + statusColor + ';">' + statusText + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;">' + downloads + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-size:12px;">' + expiresLabel + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;text-align:center;">';
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
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', '/api/shares/' + token, {}, null);
    } else {
      var resp = await fetch(BASE + '/api/shares/' + token, { method: 'DELETE', headers: headers() });
      if (!resp.ok) {
        var data = await resp.json().catch(function() { return {}; });
        showToast('撤销失败: ' + (data.message || resp.status), 'error');
        return;
      }
    }
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
let _cloudPollTimer = null;

function showCloudDownload() {
  document.getElementById('cloud-modal').style.display = 'flex';
  refreshCloudTasks();
  startCloudPolling();
}

function hideCloudDownload() {
  document.getElementById('cloud-modal').style.display = 'none';
  stopCloudPolling();
}

function startCloudPolling() {
  stopCloudPolling();
  _cloudPollTimer = setInterval(refreshCloudTasks, 3000);
}

function stopCloudPolling() {
  if (_cloudPollTimer) { clearInterval(_cloudPollTimer); _cloudPollTimer = null; }
}

async function refreshCloudTasks() {
  const body = document.getElementById('cloud-tasks-body');
  try {
    let tasks;
    const url = '/api/cloud/tasks';
    if (tunnelHexKey) {
      const result = await tunnelRequest('GET', url, {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      tasks = data || [];
    } else {
      const resp = await fetch(BASE + url, { headers: headers() });
      if (!resp.ok) { body.innerHTML = '<div class="empty-msg">请求失败: ' + resp.status + '</div>'; return; }
      tasks = await resp.json();
    }
    _cloudTasks = tasks || [];
    if (_cloudTasks.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无下载任务</div>';
      return;
    }
    body.innerHTML = buildCloudTaskTableHtml(_cloudTasks);
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  }
}

async function createCloudTask() {
  const input = document.getElementById('cloud-url');
  const text = input.value.trim();
  if (!text) { showToast('请输入下载链接', 'warning'); return; }

  const lines = text.split('\n').map(function(l) { return l.trim(); }).filter(function(l) { return l.length > 0; });
  if (lines.length === 0) { showToast('请输入下载链接', 'warning'); return; }

  try {
    const hdrs = headers({ 'Content-Type': 'application/json' });
    input.value = '';

    if (lines.length === 1) {
      // 单 URL：使用原有 API
      let task;
      if (tunnelHexKey) {
        const result = await tunnelRequest('POST', '/api/cloud/download', hdrs, JSON.stringify({ url: lines[0] }));
        task = JSON.parse(new TextDecoder().decode(result.body));
      } else {
        const resp = await fetch(BASE + '/api/cloud/download', { method: 'POST', headers: hdrs, body: JSON.stringify({ url: lines[0] }) });
        task = await resp.json();
        if (!resp.ok) { showToast('创建失败: ' + (task.error || resp.status), 'error'); return; }
      }
      showToast('任务已创建: ' + task.id, 'success');
    } else {
      // 多 URL：使用批量 API
      const urls = lines.map(function(url) { return { url: url }; });
      let data;
      if (tunnelHexKey) {
        const result = await tunnelRequest('POST', '/api/cloud/download/batch', hdrs, JSON.stringify({ urls: urls }));
        data = JSON.parse(new TextDecoder().decode(result.body));
      } else {
        const resp = await fetch(BASE + '/api/cloud/download/batch', { method: 'POST', headers: hdrs, body: JSON.stringify({ urls: urls }) });
        data = await resp.json();
        if (!resp.ok) { showToast('创建失败: ' + (data.error || resp.status), 'error'); return; }
      }
      const tasks = data.tasks || [];
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

async function downloadCloudFile(taskId, filename, checksum) {
  try {
    // 先下载云端文件
    const cloudPath = '.__cloud__/' + taskId + '/' + filename;
    const downloadUrl = '/download?filename=' + encodeURIComponent(cloudPath);
    let buffer, serverCS;
    if (tunnelHexKey) {
      const result = await tunnelDownloadStream(cloudPath);
      if (result) {
        buffer = result.body;
        serverCS = (result.headers['X-File-Checksum'] || [''])[0];
      } else {
        const result2 = await tunnelRequest('GET', downloadUrl, {}, null);
        buffer = result2.body;
        serverCS = (result2.headers['X-File-Checksum'] || [''])[0];
      }
    } else {
      const resp = await fetch(BASE + downloadUrl, { headers: headers() });
      if (!resp.ok) { showToast('下载失败: HTTP ' + resp.status, 'error'); return; }
      buffer = await resp.arrayBuffer();
      serverCS = resp.headers.get('X-File-Checksum') || checksum;
    }

    // 校验 checksum
    if (serverCS) {
      const sha256 = new Sha256();
      sha256.update(new Uint8Array(buffer));
      const localCS = sha256.digest();
      if (localCS !== serverCS) {
        showToast('校验失败: ' + filename, 'error');
        return;
      }
    }

    // 触发浏览器下载
    const blob = new Blob([buffer], { type: 'application/octet-stream' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    a.click();
    URL.revokeObjectURL(a.href);
    showToast('下载完成: ' + filename, 'success');

    // 清理云端副本
    await deleteCloudTask(taskId, filename, serverCS);
  } catch (e) { showToast('下载失败: ' + e.message, 'error'); }
}

async function deleteCloudTask(taskId, filename, checksum) {
  try {
    // 删除云端文件
    const cloudPath = '.__cloud__/' + taskId + '/' + filename;
    if (tunnelHexKey) {
      await tunnelRequest('POST', '/delete?filename=' + encodeURIComponent(cloudPath), { 'X-File-Checksum': checksum }, null);
      await tunnelRequest('DELETE', '/api/cloud/tasks/' + taskId, {}, null);
    } else {
      const hdrs = headers({ 'X-File-Checksum': checksum });
      await fetch(BASE + '/delete?filename=' + encodeURIComponent(cloudPath), { method: 'POST', headers: hdrs });
      await fetch(BASE + '/api/cloud/tasks/' + taskId, { method: 'DELETE', headers: headers() });
    }
    refreshCloudTasks();
  } catch (e) { /* 静默处理 */ }
}

async function cancelCloudTask(taskId) {
  try {
    const url = '/api/cloud/tasks/' + taskId + '/cancel';
    if (tunnelHexKey) {
      await tunnelRequest('POST', url, {}, null);
    } else {
      await fetch(BASE + url, { method: 'POST', headers: headers() });
    }
    showToast('任务已取消', 'success');
    refreshCloudTasks();
  } catch (e) { showToast('取消失败: ' + e.message, 'error'); }
}

async function removeCloudTask(taskId) {
  try {
    const url = '/api/cloud/tasks/' + taskId;
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', url, {}, null);
    } else {
      await fetch(BASE + url, { method: 'DELETE', headers: headers() });
    }
    showToast('任务已删除', 'success');
    refreshCloudTasks();
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

function buildCloudTaskTableHtml(tasks) {
  let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">文件名</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">状态</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">大小</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">操作</th></tr></thead><tbody>';
  for (const t of tasks) {
    const statusLabel = statusText(t.status);
    const rowClass = t.status === 'downloading' ? ' style="background:#f0f4ff;"' : '';
    html += '<tr' + rowClass + '><td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + escHtml(t.filename || '') + '">' + escHtml(t.filename || '-') + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;">' + statusLabel + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' + (t.total_size > 0 ? formatSize(t.total_size) : '-') + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' +
      cloudTaskActions(t.id, t.filename, t.status, t.checksum) + '</td></tr>';
  }
  html += '</tbody></table>';
  return html;
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

async function loadVersions() {
  var filename = document.getElementById('version-filename').value.trim();
  if (!filename) { showToast('请输入文件名', 'warning'); return; }
  var body = document.getElementById('version-body');
  body.innerHTML = '<div class="empty-msg">加载中...</div>';
  try {
    var versions;
    var url = '/api/versions?filename=' + encodeURIComponent(filename);
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', url, {}, null);
      var data = JSON.parse(new TextDecoder().decode(result.body));
      versions = data.versions || [];
    } else {
      var resp = await fetch(BASE + url, { headers: headers() });
      if (!resp.ok) {
        var errData = await resp.json().catch(function() { return {}; });
        if (resp.status === 501) { body.innerHTML = '<div class="empty-msg">版本管理未启用（需在配置中设置 versioning.enabled: true）</div>'; return; }
        body.innerHTML = '<div class="empty-msg">加载失败: ' + (errData.message || resp.status) + '</div>'; return;
      }
      var data = await resp.json();
      versions = data.versions || [];
    }
    if (versions.length === 0) { body.innerHTML = '<div class="empty-msg">该文件没有版本历史</div>'; return; }
    body.innerHTML = buildVersionTableHtml(versions, filename);
  } catch (e) { body.innerHTML = '<div class="empty-msg">加载失败: ' + e.message + '</div>'; }
}

function buildVersionTableHtml(versions, filename) {
  var html = '<div style="margin-bottom:8px;font-size:13px;color:#666;">文件: <strong>' + escHtml(filename) + '</strong>，共 ' + versions.length + ' 个版本</div>';
  html += '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">版本 ID</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">时间</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">大小</th>' +
    '<th style="text-align:right;padding:4px 8px;border-bottom:1px solid #eee;">操作</th></tr></thead><tbody>';
  for (var i = 0; i < versions.length; i++) {
    var v = versions[i];
    var versionTime = v.created_at || '-';
    html += '<tr><td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;font-family:monospace;font-size:12px;">' + escHtml(String(v.version_id || '-')) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' + escHtml(versionTime) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' + formatSize(v.size) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;text-align:right;white-space:nowrap;">' +
      '<button class="btn btn-primary btn-sm version-restore-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '" style="margin-right:4px;">恢复</button>' +
      '<button class="btn btn-danger btn-sm version-delete-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '">删除</button></td></tr>';
  }
  html += '</tbody></table>';
  return html;
}

async function restoreVersion(filename, versionId) {
  if (!confirm('确认恢复版本 ' + versionId + ' ？\n当前文件将被备份为新版本。')) return;
  try {
    var url = '/api/versions/restore?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(versionId);
    if (tunnelHexKey) {
      var result = await tunnelRequest('POST', url, {}, null);
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('版本恢复成功', 'success'); loadVersions(); refreshList(); }
      else { showToast('恢复失败: ' + (data.message || 'unknown'), 'error'); }
    } else {
      var resp = await fetch(BASE + url, { method: 'POST', headers: headers() });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('版本恢复成功', 'success'); loadVersions(); refreshList(); }
      else { showToast('恢复失败: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('恢复失败: ' + e.message, 'error'); }
}

async function deleteVersion(filename, versionId) {
  if (!confirm('确认删除版本 ' + versionId + ' ？\n此操作不可恢复。')) return;
  try {
    var url = '/api/versions?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(versionId);
    if (tunnelHexKey) {
      var result = await tunnelRequest('DELETE', url, {}, null);
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('版本已删除', 'success'); loadVersions(); }
      else { showToast('删除失败: ' + (data.message || 'unknown'), 'error'); }
    } else {
      var resp = await fetch(BASE + url, { method: 'DELETE', headers: headers() });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('版本已删除', 'success'); loadVersions(); }
      else { showToast('删除失败: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('删除失败: ' + e.message, 'error'); }
}

// --- DOMContentLoaded 初始化：用 addEventListener 绑定所有静态 HTML 元素 ---
document.addEventListener('DOMContentLoaded', function() {
  // 认证栏
  document.getElementById('save-token-btn').addEventListener('click', saveToken);
  document.getElementById('save-tunnel-key-btn').addEventListener('click', saveTunnelKey);

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

  // 云端下载弹窗
  document.getElementById('cloud-close-btn').addEventListener('click', hideCloudDownload);
  document.getElementById('cloud-url').addEventListener('keydown', function(e) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); createCloudTask(); }
  });
  document.getElementById('cloud-create-btn').addEventListener('click', createCloudTask);
  document.getElementById('cloud-refresh-btn').addEventListener('click', refreshCloudTasks);
  document.getElementById('cloud-close-modal-btn').addEventListener('click', hideCloudDownload);

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
  modal.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.8);z-index:2000;display:flex;align-items:center;justify-content:center;cursor:pointer;';

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
    var url = '/download?filename=' + encodeURIComponent(filename);
    var text;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', url, {}, null);
      text = new TextDecoder().decode(result.body);
    } else {
      var resp = await fetch(BASE + url, { headers: headers() });
      if (!resp.ok) { showToast('预览失败: ' + resp.status, 'error'); return; }
      text = await resp.text();
    }
    var lines = text.split('\n');
    if (lines.length > 100) {
      text = lines.slice(0, 100).join('\n') + '\n\n... (共 ' + lines.length + ' 行，仅显示前 100 行)';
    }
    showTextPreview(filename, text);
  } catch (e) { showToast('预览失败: ' + e.message, 'error'); }
}

function showTextPreview(filename, text) {
  var modal = document.createElement('div');
  modal.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:2000;display:flex;align-items:center;justify-content:center;';

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
