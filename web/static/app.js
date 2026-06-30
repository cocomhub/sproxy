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
        container.innerHTML = '<button class="btn btn-primary" onclick="loadMore()">加载更多 (' + remaining + ')</button>';
      } else {
        container.innerHTML = '<div style="text-align:center;padding:12px;color:#999;">已加载全部 ' + data.total + ' 个文件</div>';
      }
    }
  } catch { /* 静默处理 */ }
}

function buildFileTableHtml(files, subdir) {
  let html = '<table id="file-table"><thead><tr><th class="check-col"><input type="checkbox" id="select-all-checkbox" onchange="toggleSelectAll(this.checked)"></th><th>文件名</th><th>大小</th><th>Checksum (SHA-256)</th><th>操作</th></tr></thead><tbody>';
  for (const fi of files) {
    const fullName = subdir ? subdir + '/' + fi.name : fi.name;
    html += buildFileRowHtml(fi, fullName);
  }
  html += '</tbody></table>';
  return html;
}

function buildFileRowHtml(fi, fullName) {
  if (fi.is_dir) {
    return '<tr style="cursor:pointer;background:#f8f9fa;" class="dir-row"><td class="check-col"></td><td onclick="navigateDir(\'' + escJsStr(fullName) + '\')"><strong>' + escHtml(fi.name) + '/</strong></td>' +
      '<td>-</td><td>-</td><td>' +
      '<button class="btn btn-sm btn-secondary dir-enter-btn" onclick="event.stopPropagation();navigateDir(\'' + escJsStr(fullName) + '\')">进入</button>' +
      '<button class="btn btn-sm btn-danger dir-delete-btn" onclick="event.stopPropagation();rmdirDir(\'' + escJsStr(fullName) + '\')">删除</button></td></tr>';
  }
  const cs = fi.checksum || '';
  const csDisplay = cs ? '<span class="checksum-cell" title="' + escHtml(cs) + '" onclick="copyChecksum(\'' + escHtml(cs) + '\')">' + escHtml(getChecksumPrefix(cs)) + '<span class="copy-icon">📋</span></span>' : '-';
  return '<tr><td class="check-col"><input type="checkbox" class="file-select" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '" onchange="updateBatchToolbar()"></td><td class="overflow-dots" title="' + escHtml(fullName) + '">' + escHtml(fi.name) + '</td>' +
    '<td class="size-cell">' + formatSize(fi.size) + '</td>' +
    '<td>' + csDisplay + '</td>' +
    '<td class="file-actions">' +
    '<button class="btn btn-primary btn-sm file-download-btn" onclick="downloadFile(\'' + escJsStr(fullName) + '\', \'' + escJsStr(cs) + '\')">下载</button>' +
    '<button class="btn btn-danger btn-sm file-delete-btn" onclick="deleteFile(\'' + escJsStr(fullName) + '\', \'' + escJsStr(cs) + '\')">删除</button>' +
    '<button class="btn btn-warning btn-sm file-rename-btn" onclick="renameFile(\'' + escJsStr(fullName) + '\', \'' + escJsStr(cs) + '\')">重命名</button>' +
    '</td></tr>';
}

function buildLoadMoreHtml(total) {
  if (!_hasMore) return '';
  const remaining = (total || 0) - _currentOffset;
  return '<div id="load-more-container" style="text-align:center;padding:12px;">' +
    '<button class="btn btn-primary" onclick="loadMore()">加载更多 (' + remaining + ')</button></div>';
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
    el.innerHTML = '<a href="javascript:void(0)" onclick="navigateDir(\'\')">/</a>';
    return;
  }
  const parts = currentSubdir.split('/');
  let html = '<a href="javascript:void(0)" onclick="navigateDir(\'\')">/</a>';
  let accumulated = '';
  for (const p of parts) {
    accumulated = accumulated ? accumulated + '/' + p : p;
    html += ' <span style="color:#999">›</span> <a href="javascript:void(0)" onclick="navigateDir(\'' + escJsStr(accumulated) + '\')">' + escHtml(p) + '</a>';
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

// --- 监控 ---
async function showStats() {
  document.getElementById('stats-modal').style.display = 'flex';
  document.getElementById('stats-body').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">加载中...</div>';
  try {
    const hdrs = token ? { 'Authorization': 'Bearer ' + token } : {};
    const resp = await fetch(BASE + '/api/stats', { headers: hdrs });
    if (!resp.ok) { document.getElementById('stats-body').innerHTML = '<div style="color:red">请求失败: ' + resp.status + '</div>'; return; }
    const s = await resp.json();
    const du = s.disk_usage || {};
    const rc = s.request_counts || {};
    document.getElementById('stats-body').innerHTML = statsTableHtml(du, rc, s);
  } catch (e) { document.getElementById('stats-body').innerHTML = '<div style="color:red">错误: ' + e.message + '</div>'; }
}

function hideStats() {
  document.getElementById('stats-modal').style.display = 'none';
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
    '<tr><td style="padding:5px 0;color:#777">删除文件数</td><td style="content/rich:right">' + (s.files_deleted ?? 0) + '</td></tr></table>';
}

// --- 初始化 ---
refreshList();
checkResumableUploads();

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
    actions += '<button class="btn btn-primary btn-sm" onclick="downloadCloudFile(\'' + escJsStr(id) + '\',\'' + escJsStr(filename) + '\',\'' + escJsStr(checksum || '') + '\')" style="margin-right:4px;">下载到本地</button>';
    actions += '<button class="btn btn-danger btn-sm" onclick="removeCloudTask(\'' + escJsStr(id) + '\')">删除</button>';
  } else if (status === 'failed' || status === 'cancelled') {
    actions += '<button class="btn btn-danger btn-sm" onclick="removeCloudTask(\'' + escJsStr(id) + '\')">删除</button>';
  } else {
    actions += '<button class="btn btn-warning btn-sm" onclick="cancelCloudTask(\'' + escJsStr(id) + '\')">取消</button>';
  }
  return actions;
}
