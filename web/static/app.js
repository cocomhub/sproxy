// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 涓婚€昏緫锛氭枃浠跺垪琛ㄣ€丆RUD銆佹壒閲忔搷浣溿€佸鑸€乁I 宸ュ叿銆?// 渚濊禆 sha256.js, tunnel.js, upload.js锛堝厛鍔犺浇锛夈€?
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
  showToast('Token 宸蹭繚瀛?, 'success');
}

function saveTunnelKey() {
  tunnelHexKey = document.getElementById('tunnel-key').value;
  localStorage.setItem('sproxy_tunnel_key', tunnelHexKey);
  _tunnelCryptoKey = null;
  showToast('Tunnel Key 宸蹭繚瀛?, 'success');
}

// --- UI 宸ュ叿 ---
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
  return cs.substring(0, 16) + '鈥?;
}

function copyChecksum(cs) {
  navigator.clipboard.writeText(cs).then(function() {
    showToast('Checksum 宸插鍒跺埌鍓创鏉?, 'success');
  }).catch(function() {
    showToast('澶嶅埗澶辫触', 'error');
  });
}

// --- 鏂囦欢鍒楄〃 ---
async function refreshList() {
  const el = document.getElementById('file-list');
  el.innerHTML = '<div class="empty-msg">鍔犺浇涓?..</div>';
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
      if (!resp.ok) { el.innerHTML = '<div class="empty-msg">鍔犺浇澶辫触: ' + escHtml(data.message || String(resp.status)) + '</div>'; return; }
      files = data.files || [];
    }
    _currentOffset = files.length;
    _hasMore = (data.total || 0) > _currentOffset;
    if (files.length === 0) { el.innerHTML = '<div class="empty-msg">鏆傛棤鏂囦欢</div>'; return; }
    el.innerHTML = buildFileTableHtml(files, currentSubdir) + buildLoadMoreHtml(data.total);
    updateBatchToolbar();
  } catch (e) {
    el.innerHTML = '<div class="empty-msg">璇锋眰澶辫触: ' + e.message + '</div>';
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
        container.innerHTML = '<button class="btn btn-primary">鍔犺浇鏇村 (' + remaining + ')</button>';
      } else {
        container.innerHTML = '<div style="text-align:center;padding:12px;color:#999;">宸插姞杞藉叏閮?' + data.total + ' 涓枃浠?/div>';
      }
    }
  } catch { /* 闈欓粯澶勭悊 */ }
}

function buildFileTableHtml(files, subdir) {
  let html = '<table id="file-table"><thead><tr><th class="check-col"><input type="checkbox" id="select-all-checkbox"></th><th>鏂囦欢鍚?/th><th>澶у皬</th><th>Checksum (SHA-256)</th><th>鎿嶄綔</th></tr></thead><tbody>';
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
      '<button class="btn btn-sm btn-secondary dir-enter-btn" data-subdir="' + escHtml(fullName) + '">杩涘叆</button>' +
      '<button class="btn btn-sm btn-primary dir-archive-btn" data-subdir="' + escHtml(fullName) + '">鎵撳寘涓嬭浇</button>' +
      '<button class="btn btn-sm btn-danger dir-delete-btn" data-subdir="' + escHtml(fullName) + '">鍒犻櫎</button></td></tr>';
  }
  const cs = fi.checksum || '';
  const csDisplay = cs ? '<span class="checksum-cell" data-checksum="' + escHtml(cs) + '" title="' + escHtml(cs) + '">' + escHtml(getChecksumPrefix(cs)) + '<span class="copy-icon">馃搵</span></span>' : '-';
  return '<tr><td class="check-col"><input type="checkbox" class="file-select" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '"></td><td class="overflow-dots" title="' + escHtml(fullName) + '">' + escHtml(fi.name) + '</td>' +
    '<td class="size-cell">' + formatSize(fi.size) + '</td>' +
    '<td>' + csDisplay + '</td>' +
    '<td class="file-actions">' +
    '<button class="btn btn-primary btn-sm file-download-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">涓嬭浇</button>' +
    '<button class="btn btn-sm btn-secondary file-preview-btn" data-filename="' + escHtml(fullName) + '">棰勮</button>' +
    '<button class="btn btn-danger btn-sm file-delete-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">鍒犻櫎</button>' +
    '<button class="btn btn-warning btn-sm file-rename-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">閲嶅懡鍚?/button>' +
    '<button class="btn btn-sm btn-share file-share-btn" data-filename="' + escHtml(fullName) + '" data-checksum="' + escHtml(cs) + '">鍒嗕韩</button>' +
    '</td></tr>';
}

function buildLoadMoreHtml(total) {
  if (!_hasMore) return '';
  const remaining = (total || 0) - _currentOffset;
  return '<div id="load-more-container" style="text-align:center;padding:12px;">' +
    '<button class="btn btn-primary">鍔犺浇鏇村 (' + remaining + ')</button></div>';
}

// --- 鎼滅储 ---
async function searchFiles() {
  const q = document.getElementById('search-input').value.trim();
  if (!q) { clearSearch(); return; }
  const el = document.getElementById('file-list');
  el.innerHTML = '<div class="empty-msg">鎼滅储涓?..</div>';
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
        el.innerHTML = '<div class="empty-msg">鎼滅储澶辫触: ' + (errData.message || resp.status) + '</div>';
        return;
      }
      const data = await resp.json();
      files = data.files || [];
    }
    _searchActive = true;
    document.getElementById('clear-search-btn').style.display = '';
    if (files.length === 0) { el.innerHTML = '<div class="empty-msg">鏈壘鍒板尮閰嶆枃浠?/div>'; return; }
    el.innerHTML = buildFileTableHtml(files, '');
    updateBatchToolbar();
  } catch (e) {
    el.innerHTML = '<div class="empty-msg">鎼滅储澶辫触: ' + e.message + '</div>';
  }
}

function clearSearch() {
  document.getElementById('search-input').value = '';
  document.getElementById('clear-search-btn').style.display = 'none';
  _searchActive = false;
  refreshList();
}

// --- 鐩綍瀵艰埅 ---
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
    html += ' <span style="color:#999">鈥?/span> <a href="#" data-subdir="' + escHtml(accumulated) + '">' + escHtml(p) + '</a>';
  }
  el.innerHTML = html;
}

// --- 鐩綍鎿嶄綔 ---
async function mkdirDir() {
  const input = document.getElementById('new-dir-name');
  const name = input.value.trim();
  if (!name) { showToast('璇疯緭鍏ョ洰褰曞悕', 'warning'); return; }
  const dirPath = currentSubdir ? currentSubdir + '/' + name : name;
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/mkdir?dirname=' + encodeURIComponent(dirPath), {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) {
        showToast('鐩綍宸插垱寤? ' + dirPath, 'success');
        input.value = '';
        refreshList();
      } else { showToast('鍒涘缓鐩綍澶辫触: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/mkdir?dirname=' + encodeURIComponent(dirPath), { method: 'POST', headers: headers() });
      const data = await resp.json();
      if (resp.ok && data.success) {
        showToast('鐩綍宸插垱寤? ' + dirPath, 'success');
        input.value = '';
        refreshList();
      } else { showToast('鍒涘缓鐩綍澶辫触: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('鍒涘缓鐩綍澶辫触: ' + e.message, 'error'); }
}

async function rmdirDir(dirPath) {
  if (!confirm('纭鍒犻櫎鐩綍 "' + dirPath + '" 鍙婂叾鎵€鏈夊唴瀹?')) return;
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/rmdir?dirname=' + encodeURIComponent(dirPath), {}, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('鐩綍宸插垹闄? ' + dirPath, 'success'); refreshList(); }
      else { showToast('鍒犻櫎鐩綍澶辫触: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/rmdir?dirname=' + encodeURIComponent(dirPath), { method: 'POST', headers: headers() });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('鐩綍宸插垹闄? ' + dirPath, 'success'); refreshList(); }
      else { showToast('鍒犻櫎鐩綍澶辫触: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('鍒犻櫎鐩綍澶辫触: ' + e.message, 'error'); }
}

// --- 涓嬭浇 ---
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
          showToast(name + ' 鏍￠獙澶辫触: 鏈嶅姟绔?' + serverCS.substring(0, 16) + '鈥? 鏈湴 ' + localCS.substring(0, 16) + '鈥?, 'error');
          return;
        }
      }
      triggerDownload(name, result.body);
      showToast(name + ' 涓嬭浇瀹屾垚' + (serverCS ? '锛屾牎楠岄€氳繃' : ''), 'success');
    } else {
      await directDownload(name);
    }
  } catch (e) { showToast('涓嬭浇澶辫触: ' + e.message, 'error'); }
}

async function directDownload(name) {
  const resp = await fetch(BASE + '/download?filename=' + encodeURIComponent(name), { headers: headers() });
  if (!resp.ok) {
    const data = await resp.json().catch(function() { return {}; });
    showToast('涓嬭浇澶辫触: ' + (data.message || resp.status), 'error');
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
        showToast(name + ' 鏍￠獙澶辫触: 鏈嶅姟绔?' + serverCS.substring(0, 16) + '鈥? 鏈湴 ' + localCS.substring(0, 16) + '鈥?, 'error');
        return;
      }
      const resp2 = await fetch(BASE + '/download?filename=' + encodeURIComponent(name), { headers: headers() });
      triggerDownload(name, await resp2.blob());
      showToast(name + ' 涓嬭浇瀹屾垚锛屾牎楠岄€氳繃', 'success');
      return;
    }
    const buffer = await resp.arrayBuffer();
    sha256.update(new Uint8Array(buffer));
    const localCS = sha256.digest();
    if (localCS !== serverCS) {
      showToast(name + ' 鏍￠獙澶辫触: 鏈嶅姟绔?' + serverCS.substring(0, 16) + '鈥? 鏈湴 ' + localCS.substring(0, 16) + '鈥?, 'error');
      return;
    }
    triggerDownload(name, buffer);
    showToast(name + ' 涓嬭浇瀹屾垚锛屾牎楠岄€氳繃', 'success');
    return;
  }
  triggerDownload(name, await resp.blob());
  showToast(name + ' 涓嬭浇瀹屾垚', 'success');
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

// --- 鍒犻櫎 ---
async function deleteFile(name, checksum) {
  if (!confirm('纭鍒犻櫎 "' + name + '"?')) return;
  if (!checksum) { showToast('缂哄皯 checksum锛屾棤娉曟牎楠屽畬鏁存€?, 'error'); return; }
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/delete?filename=' + encodeURIComponent(name), { 'X-File-Checksum': checksum }, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('鍒犻櫎鎴愬姛: ' + name, 'success'); refreshList(); }
      else { showToast('鍒犻櫎澶辫触: ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/delete?filename=' + encodeURIComponent(name), {
        method: 'POST', headers: headers({ 'X-File-Checksum': checksum })
      });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('鍒犻櫎鎴愬姛: ' + name, 'success'); refreshList(); }
      else { showToast('鍒犻櫎澶辫触: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('鍒犻櫎澶辫触: ' + e.message, 'error'); }
}

// --- 閲嶅懡鍚?---
async function renameFile(name, checksum) {
  if (!checksum) { showToast('缂哄皯 checksum锛屾棤娉曟牎楠屽畬鏁存€?, 'error'); return; }
  const newName = prompt('鏂扮殑鏂囦欢鍚嶏紙璺緞锛?', name);
  if (!newName || newName === name) return;
  try {
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/rename?from=' + encodeURIComponent(name) + '&to=' + encodeURIComponent(newName), { 'X-File-Checksum': checksum }, null);
      const data = JSON.parse(new TextDecoder().decode(result.body));
      if (result.status >= 200 && result.status < 300 && data.success) { showToast('閲嶅懡鍚嶆垚鍔? ' + newName, 'success'); refreshList(); }
      else { showToast('閲嶅懡鍚嶅け璐? ' + (data.message || result.status), 'error'); }
    } else {
      const resp = await fetch(BASE + '/rename?from=' + encodeURIComponent(name) + '&to=' + encodeURIComponent(newName), {
        method: 'POST', headers: headers({ 'X-File-Checksum': checksum })
      });
      const data = await resp.json();
      if (resp.ok && data.success) { showToast('閲嶅懡鍚嶆垚鍔? ' + newName, 'success'); refreshList(); }
      else { showToast('閲嶅懡鍚嶅け璐? ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('閲嶅懡鍚嶅け璐? ' + e.message, 'error'); }
}

// --- 鎵归噺鎿嶄綔 ---
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
  label.textContent = '宸查€?' + count + ' 涓枃浠?;
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
  if (files.length === 0) { showToast('璇峰厛閫夋嫨鏂囦欢', 'error'); return; }
  if (!confirm('纭畾瑕佸垹闄ら€変腑鐨?' + files.length + ' 涓枃浠跺悧锛?)) return;
  const body = JSON.stringify({ files: files });
  try {
    const data = await sendBatchRequest('/api/batch/delete', body);
    if (data.success) { showToast(data.message || '鍒犻櫎瀹屾垚', 'success'); refreshList(); }
    else { showToast(data.message || '鎵归噺鍒犻櫎澶辫触', 'error'); }
  } catch (e) { showToast('鎵归噺鍒犻櫎澶辫触: ' + e.message, 'error'); }
}

async function batchRename() {
  const files = getSelectedFiles();
  if (files.length === 0) { showToast('璇峰厛閫夋嫨鏂囦欢', 'error'); return; }
  const operations = [];
  for (const f of files) {
    const newName = prompt('閲嶅懡鍚?"' + f.filename + '"\n璇疯緭鍏ユ柊鏂囦欢鍚嶏紙鍙栨秷璺宠繃锛?', f.filename);
    if (newName === null) continue;
    if (newName.trim() === '') { showToast('鏂囦欢鍚嶄笉鑳戒负绌?, 'error'); return; }
    if (newName === f.filename) continue;
    operations.push({ from: f.filename, to: newName, checksum: f.checksum });
  }
  if (operations.length === 0) { showToast('娌℃湁闇€瑕侀噸鍛藉悕鐨勬枃浠?, 'info'); return; }
  try {
    const data = await sendBatchRequest('/api/batch/rename', JSON.stringify({ operations: operations }));
    if (data.success) { showToast(data.message || '閲嶅懡鍚嶅畬鎴?, 'success'); clearSelection(); refreshList(); }
    else { showToast(data.message || '鎵归噺閲嶅懡鍚嶅け璐?, 'error'); }
  } catch (e) { showToast('鎵归噺閲嶅懡鍚嶅け璐? ' + e.message, 'error'); }
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
  if (selected.length === 0) { showToast('璇烽€夋嫨鏂囦欢', 'warning'); return; }
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
    return resp.blob().then(function(blob) { triggerDownload(filename, blob); showToast('褰掓。涓嬭浇瀹屾垚: ' + filename, 'success'); });
  }).catch(function(err) { showToast('褰掓。澶辫触: ' + err.message, 'error'); });
}

// 鐩綍鎵撳寘涓嬭浇锛圙ET /api/archive-dir锛?async function downloadDirArchive(dirPath) {
  try {
    var url = '/api/archive-dir?dirname=' + encodeURIComponent(dirPath);
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', url, {}, null);
      triggerDownload(dirPath.replace('/', '_') + '.tar.gz', result.body);
      showToast('鐩綍鎵撳寘涓嬭浇瀹屾垚', 'success');
    } else {
      var resp = await fetch(BASE + url, { headers: headers() });
      if (!resp.ok) {
        var errData = await resp.json().catch(function() { return {}; });
        showToast('鎵撳寘涓嬭浇澶辫触: ' + (errData.message || resp.status), 'error');
        return;
      }
      var disposition = resp.headers.get('Content-Disposition') || '';
      var match = disposition.match(/filename="?(.+?)"?$/);
      var filename = match ? match[1] : dirPath.replace('/', '_') + '.tar.gz';
      var blob = await resp.blob();
      triggerDownload(filename, blob);
      showToast('鐩綍鎵撳寘涓嬭浇瀹屾垚: ' + filename, 'success');
    }
  } catch (e) { showToast('鎵撳寘涓嬭浇澶辫触: ' + e.message, 'error'); }
}

// --- 鐩戞帶 ---
async function showStats() {
  document.getElementById('stats-modal').style.display = 'flex';
  switchStatsTab('stats');
  document.getElementById('stats-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">鍔犺浇涓?..</div>';
  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/stats', {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
    } else {
      var resp = await fetch(BASE + '/api/stats', { headers: headers() });
      if (!resp.ok) { document.getElementById('stats-panel').innerHTML = '<div style="color:red">璇锋眰澶辫触: ' + resp.status + '</div>'; return; }
      data = await resp.json();
    }
    var du = data.disk_usage || {};
    var rc = data.request_counts || {};
    document.getElementById('stats-panel').innerHTML = statsTableHtml(du, rc, data);
  } catch (e) { document.getElementById('stats-panel').innerHTML = '<div style="color:red">閿欒: ' + e.message + '</div>'; }
}

function hideStats() {
  document.getElementById('stats-modal').style.display = 'none';
}

// --- 鐩戞帶寮圭獥鏍囩椤靛垏鎹?---
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
  document.getElementById('config-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">鍔犺浇涓?..</div>';
  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/config', {}, null);
      data = JSON.parse(new TextDecoder().decode(result.body));
    } else {
      var resp = await fetch(BASE + '/api/config', { headers: headers() });
      if (!resp.ok) { document.getElementById('config-panel').innerHTML = '<div style="color:red">璇锋眰澶辫触: ' + resp.status + '</div>'; return; }
      data = await resp.json();
    }
    document.getElementById('config-panel').innerHTML = configTableHtml(data);
  } catch (e) { document.getElementById('config-panel').innerHTML = '<div style="color:red">閿欒: ' + e.message + '</div>'; }
}

// --- Hub 绠＄悊 ---
async function showHub() {
  document.getElementById('hub-panel').innerHTML = '<div style="text-align:center;padding:20px;color:#999;">鍔犺浇涓?..</div>';
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
        document.getElementById('hub-panel').innerHTML = '<div class="empty-msg">Hub 鏈惎鐢ㄦ垨璇锋眰澶辫触</div>';
        return;
      }
      nodes = await nResp.json();
      stats = await sResp.json();
    }
    document.getElementById('hub-panel').innerHTML = hubTableHtml(nodes, stats);
  } catch (e) {
    document.getElementById('hub-panel').innerHTML = '<div class="empty-msg">Hub 鏈惎鐢ㄦ垨璇锋眰澶辫触: ' + e.message + '</div>';
  }
}

function hubTableHtml(nodes, stats) {
  var html = '';
  // 缁熻姒傝
  if (stats) {
    html += '<div style="margin-bottom:12px;padding:8px 12px;background:#f0f8ff;border-radius:4px;font-size:13px;">';
    html += '宸茶繛鎺ヨ妭鐐? <strong>' + (stats.nodes_connected ?? 0) + '</strong></div>';
  }

  if (!nodes || nodes.length === 0) {
    html += '<div class="empty-msg">鏆傛棤宸茶繛鎺ヨ妭鐐?/div>';
    return html;
  }

  html += '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
  html += '<thead><tr style="background:#f5f5f5;">';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">鑺傜偣 ID</th>';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">鍦板潃</th>';
  html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">杩炴帴鏃堕棿</th>';
  html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid #ddd;">鎿嶄綔</th>';
  html += '</tr></thead><tbody>';

  for (var i = 0; i < nodes.length; i++) {
    var n = nodes[i];
    var connected = n.connected ? new Date(n.connected).toLocaleString() : '-';
    html += '<tr>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-family:monospace;font-size:12px;">' + escHtml(n.id) + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;">' + escHtml(n.addr || '-') + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-size:12px;">' + connected + '</td>';
    html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;text-align:center;">';
    html += '<button class="btn btn-danger btn-sm hub-remove-btn" data-node-id="' + escHtml(n.id) + '">绉婚櫎</button>';
    html += '</td></tr>';
  }
  html += '</tbody></table>';
  return html;
}

async function removeHubNode(nodeId) {
  if (!confirm('纭畾绉婚櫎鑺傜偣 ' + nodeId + '锛?)) return;
  try {
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', '/api/hub/nodes/' + encodeURIComponent(nodeId), {}, null);
    } else {
      var resp = await fetch(BASE + '/api/hub/nodes/' + encodeURIComponent(nodeId), { method: 'DELETE', headers: headers() });
      if (!resp.ok) {
        var data = await resp.json().catch(function() { return {}; });
        showToast('绉婚櫎澶辫触: ' + (data.error || resp.status), 'error');
        return;
      }
    }
    showToast('鑺傜偣 ' + nodeId + ' 宸茬Щ闄?, 'success');
    showHub();
  } catch (e) { showToast('绉婚櫎澶辫触: ' + e.message, 'error'); }
}

function configTableHtml(cfg) {
  var html = '<table style="width:100%;border-collapse:collapse;font-size:14px;">';
  html += '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555">杩愯鏃堕厤缃?/th></tr>';

  function row(label, value) {
    return '<tr><td style="padding:5px 0;color:#777">' + label + '</td><td style="text-align:right">' + (value ?? '-') + '</td></tr>';
  }

  html += row('鏃ュ織绾у埆', cfg.log_level);
  html += row('鏃ュ織鏍煎紡', cfg.log_format);
  html += row('璁よ瘉浠ょ墝', cfg.auth_token_set ? '鉁?宸茶缃? : '鉂?鏈缃?);
  html += row('闅ч亾瀵嗛挜', cfg.tunnel_key_set ? '鉁?宸茶缃? : '鉂?鏈缃?);
  html += row('閫熺巼闄愬埗', cfg.rate_limit_requests + ' req / ' + (cfg.rate_limit_window || '-'));
  html += row('瀛樺偍涓婇檺', cfg.max_storage_bytes > 0 ? formatBytes(cfg.max_storage_bytes) : '涓嶉檺');
  html += row('鍒嗗潡澶у皬', formatBytes(cfg.chunk_size));
  html += row('涓婁紶浼氳瘽 TTL', cfg.upload_session_ttl || '-');
  html += row('鐗堟湰绠＄悊', cfg.versioning_enabled ? '鉁?鍚敤' : '鉂?鍏抽棴');
  html += row('浜戠骞跺彂', cfg.cloud_max_concurrent);
  html += row('鍦板潃', cfg.addr);
  html += row('涓婁紶鐩綍', cfg.uploads_dir);
  html += row('TLS', cfg.tls_enabled ? '鉁?鍚敤' : '鉂?鍏抽棴');
  html += row('Hub 涓户', cfg.hub_enabled ? '鉁?鍚敤' : '鉂?鍏抽棴');
  html += '</table>';

  // 閰嶇疆缂栬緫鍖?  html += '<div style="margin-top:16px;padding-top:12px;border-top:1px solid #eee;">';
  html += '<div style="font-size:13px;font-weight:600;color:#555;margin-bottom:8px;">蹇€熺紪杈?/div>';

  // 鏃ュ織绾у埆
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">鏃ュ織绾у埆:</span>';
  html += '<select id="cfg-log-level" style="padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  var levels = ['debug','info','warn','error'];
  for (var i = 0; i < levels.length; i++) {
    html += '<option value="' + levels[i] + '"' + (cfg.log_level === levels[i] ? ' selected' : '') + '>' + levels[i] + '</option>';
  }
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-level">鏇存柊</button></div>';

  // 鏃ュ織鏍煎紡
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">鏃ュ織鏍煎紡:</span>';
  html += '<select id="cfg-log-format" style="padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<option value="text"' + (cfg.log_format === 'text' ? ' selected' : '') + '>text</option>';
  html += '<option value="json"' + (cfg.log_format === 'json' ? ' selected' : '') + '>json</option>';
  html += '</select>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-log-format">鏇存柊</button></div>';

  // 閫熺巼闄愬埗
  html += '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">閫熺巼闄愬埗:</span>';
  html += '<input type="number" id="cfg-rate-limit" value="' + (cfg.rate_limit_requests ?? 10) + '" style="width:60px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<span style="font-size:12px;color:#999;">req / </span>';
  html += '<input type="text" id="cfg-rate-window" value="' + (cfg.rate_limit_window || '1s') + '" style="width:60px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;">';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-rate-limit">鏇存柊</button></div>';

  // 瀛樺偍闄愬埗
  html += '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;">';
  html += '<span style="font-size:13px;color:#777;">瀛樺偍涓婇檺:</span>';
  html += '<input type="number" id="cfg-max-storage" value="' + (cfg.max_storage_bytes ?? 0) + '" style="width:140px;padding:4px 8px;border:1px solid #ccc;border-radius:4px;font-size:13px;" min="0">';
  html += '<span style="font-size:12px;color:#999;">瀛楄妭锛?=涓嶉檺锛?/span>';
  html += '<button class="btn btn-sm btn-primary" id="cfg-update-storage">鏇存柊</button></div>';

  html += '</div>';
  return html;
}

async function updateConfigField(key, value) {
  var body = JSON.stringify((function() { var o = {}; o[key] = value; return o; })());
  try {
    if (tunnelHexKey) {
      var result = await tunnelRequest('PUT', '/api/config', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('閰嶇疆宸叉洿鏂?, 'success'); showConfig(); }
      else { showToast('鏇存柊澶辫触', 'error'); }
    } else {
      var resp = await fetch(BASE + '/api/config', {
        method: 'PUT', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('閰嶇疆宸叉洿鏂?, 'success'); showConfig(); }
      else { showToast('鏇存柊澶辫触: ' + (data.error || resp.status), 'error'); }
    }
  } catch (e) { showToast('鏇存柊澶辫触: ' + e.message, 'error'); }
}

// 鏃х増 updateStorageConfig锛屾敼鐢ㄩ厤缃潰鏉夸腑鐨?cfg-update-storage 浠ｆ浛
async function updateStorageConfig() {
  var input = document.getElementById('max-storage-input');
  var maxBytes = Number.parseInt(input.value) || 0;
  if (maxBytes < 0) { showToast('瀛樺偍闄愬埗涓嶈兘涓鸿礋鏁?, 'error'); return; }
  try {
    var body = JSON.stringify({ max_storage_bytes: maxBytes });
    if (tunnelHexKey) {
      var result = await tunnelRequest('PUT', '/api/storage/config', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('瀛樺偍闄愬埗宸叉洿鏂? ' + formatBytes(data.max_storage_bytes || 0), 'success'); }
      else { showToast('鏇存柊澶辫触', 'error'); }
    } else {
      var resp = await fetch(BASE + '/api/storage/config', {
        method: 'PUT', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('瀛樺偍闄愬埗宸叉洿鏂? ' + formatBytes(data.max_storage_bytes || 0), 'success'); }
      else { showToast('鏇存柊澶辫触: ' + (data.error || resp.status), 'error'); }
    }
  } catch (e) { showToast('鏇存柊澶辫触: ' + e.message, 'error'); }
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
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555">纾佺洏浣跨敤</th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">鐩綍</td><td style="text-align:right">' + (du.uploads_dir || '-') + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">鏂囦欢鏁?/td><td style="text-align:right">' + (du.total_files ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">鎬诲ぇ灏?/td><td style="text-align:right">' + formatBytes(du.total_size) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555;padding-top:14px">璇锋眰缁熻锛堣嚜鍚姩锛?/th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">鎬昏姹傛暟</td><td style="text-align:right">' + (rc.total ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">2xx</td><td style="text-align:right">' + (rc['2xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">4xx</td><td style="text-align:right">' + (rc['4xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">5xx</td><td style="text-align:right">' + (rc['5xx'] ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">娲昏穬杩炴帴</td><td style="text-align:right">' + (s.active_connections ?? 0) + '</td></tr>' +
    '<tr><th colspan="2" style="text-align:left;padding:8px 0;border-bottom:1px solid #eee;color:#555;padding-top:14px">浼犺緭缁熻锛堣嚜鍚姩锛?/th></tr>' +
    '<tr><td style="padding:5px 0;color:#777">涓婁紶鏂囦欢鏁?/td><td style="text-align:right">' + (s.files_uploaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">涓婁紶瀛楄妭鏁?/td><td style="text-align:right">' + formatBytes(s.bytes_uploaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">涓嬭浇鏂囦欢鏁?/td><td style="text-align:right">' + (s.files_downloaded ?? 0) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">涓嬭浇瀛楄妭鏁?/td><td style="text-align:right">' + formatBytes(s.bytes_downloaded) + '</td></tr>' +
    '<tr><td style="padding:5px 0;color:#777">鍒犻櫎鏂囦欢鏁?/td><td style="text-align:right">' + (s.files_deleted ?? 0) + '</td></tr></table>';
}

// --- 鏆楄壊妯″紡 ---
function initTheme() {
  var saved = localStorage.getItem('sproxy_theme');
  if (saved === 'dark') {
    document.documentElement.setAttribute('data-theme', 'dark');
    document.getElementById('theme-toggle-btn').textContent = '鈽€锔?;
  } else if (saved === 'light') {
    document.documentElement.removeAttribute('data-theme');
    document.getElementById('theme-toggle-btn').textContent = '馃寵';
  } else {
    // 鏈繚瀛樻椂璺熼殢绯荤粺
    if (window.matchMedia('(prefers-color-scheme: dark)').matches) {
      document.getElementById('theme-toggle-btn').textContent = '鈽€锔?;
    }
  }
}

function toggleTheme() {
  var current = document.documentElement.getAttribute('data-theme');
  if (current === 'dark') {
    document.documentElement.removeAttribute('data-theme');
    localStorage.setItem('sproxy_theme', 'light');
    document.getElementById('theme-toggle-btn').textContent = '馃寵';
  } else {
    document.documentElement.setAttribute('data-theme', 'dark');
    localStorage.setItem('sproxy_theme', 'dark');
    document.getElementById('theme-toggle-btn').textContent = '鈽€锔?;
  }
}

// --- 閿洏蹇嵎閿?---
document.addEventListener('keydown', function(e) {
  // 蹇界暐杈撳叆妗嗗唴鐨勫揩鎹烽敭
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA' || e.target.tagName === 'SELECT') return;

  switch (e.key) {
    case 'u': case 'U':
      // u: 涓婁紶鏂囦欢
      e.preventDefault();
      document.getElementById('file-input').click();
      break;
    case 'r': case 'R':
      // r: 鍒锋柊鍒楄〃锛堥潪 Ctrl+R锛?      if (!e.ctrlKey && !e.metaKey) {
        e.preventDefault();
        refreshList();
      }
      break;
    case '/':
      // /: 鎼滅储妗嗚仛鐒?      e.preventDefault();
      document.getElementById('search-input').focus();
      break;
    case 'Escape':
      // Esc: 鍏抽棴鎵€鏈夊脊绐?      hideStats();
      hideCloudDownload();
      hideVersioning();
      hideShareModal();
      break;
  }
});

// Ctrl+A: 鍏ㄩ€?鍙栨秷鍏ㄩ€?document.addEventListener('keydown', function(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'a') {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    var selectAll = document.getElementById('select-all-checkbox');
    if (selectAll) {
      e.preventDefault();
      selectAll.click();
    }
  }
});

// Delete: 鎵归噺鍒犻櫎閫変腑鏂囦欢
document.addEventListener('keydown', function(e) {
  if (e.key === 'Delete' && !e.target.tagName.match(/INPUT|TEXTAREA|SELECT/i)) {
    var batchDelete = document.getElementById('batch-delete-btn');
    if (batchDelete && batchDelete.style.display !== 'none') {
      e.preventDefault();
      batchDelete.click();
    }
  }
});

// --- 鍒濆鍖?---
initTheme();
refreshList();
checkResumableUploads();

// --- 鏂囦欢鍒嗕韩锛堟棫鐗堬紝鏀圭敤寮圭獥锛?---
function shareFile(name) {
  var ttl = prompt('鍒嗕韩鏈夋晥鏈燂紙渚嬪 1h, 24h, 7d锛岀暀绌?24h锛?', '24h');
  if (ttl === null) return;
  ttl = ttl.trim() || '24h';
  var maxDownloads = prompt('鏈€澶т笅杞芥鏁帮紙0=涓嶉檺锛?', '0');
  if (maxDownloads === null) return;
  maxDownloads = Number.parseInt(maxDownloads) || 0;
  var oneTime = confirm('涓€娆℃€у垎浜紙涓嬭浇涓€娆″悗鑷姩澶辨晥锛夛紵\n纭畾=鏄紝鍙栨秷=鍚?);
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
        if (!resp.ok) { showToast('鍒涘缓鍒嗕韩澶辫触: ' + (data.message || resp.status), 'error'); return; }
      }
      var shareUrl = location.origin + '/s/' + data.token;
      if (navigator.clipboard) {
        await navigator.clipboard.writeText(shareUrl);
        showToast('鍒嗕韩閾炬帴宸插鍒跺埌鍓创鏉? ' + shareUrl, 'success');
      } else {
        showToast('鍒嗕韩閾炬帴: ' + shareUrl, 'success');
      }
    } catch (e) { showToast('鍒涘缓鍒嗕韩澶辫触: ' + e.message, 'error'); }
  })();
}

// --- 鍒嗕韩绠＄悊 ---
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
  if (!filename) { showToast('璇疯緭鍏ユ枃浠跺悕', 'error'); return; }
  var ttl = document.getElementById('share-ttl').value.trim() || '24h';
  var maxDownloads = Number.parseInt(document.getElementById('share-max-downloads').value) || 0;
  var oneTime = document.getElementById('share-one-time').checked;

  var body = JSON.stringify({ filename: filename, ttl: ttl, max_downloads: maxDownloads, one_time: oneTime });

  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('POST', '/api/share', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.message && data.message !== 'ok') { showToast('鍒涘缓鍒嗕韩澶辫触: ' + data.message, 'error'); return; }
    } else {
      var resp = await fetch(BASE + '/api/share', {
        method: 'POST', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      data = await resp.json();
      if (!resp.ok) { showToast('鍒涘缓鍒嗕韩澶辫触: ' + (data.message || resp.status), 'error'); return; }
    }
    var shareUrl = location.origin + '/s/' + data.token;
    if (navigator.clipboard) {
      try {
        await navigator.clipboard.writeText(shareUrl);
        showToast('鍒嗕韩閾炬帴宸插鍒跺埌鍓创鏉? ' + shareUrl, 'success');
      } catch (_) {
        showToast('鍒嗕韩閾炬帴: ' + shareUrl, 'success');
      }
    } else {
      showToast('鍒嗕韩閾炬帴: ' + shareUrl, 'success');
    }
    refreshShareList();
  } catch (e) { showToast('鍒涘缓鍒嗕韩澶辫触: ' + e.message, 'error'); }
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
      if (!resp.ok) { body.innerHTML = '<div class="empty-msg">璇锋眰澶辫触: ' + resp.status + '</div>'; return; }
      shares = (await resp.json()).shares || [];
    }

    if (shares.length === 0) {
      body.innerHTML = '<div class="empty-msg">鏆傛棤鍒嗕韩閾炬帴</div>';
      return;
    }

    var html = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:#f5f5f5;"><th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">鏂囦欢鍚?/th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">鐘舵€?/th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">涓嬭浇娆℃暟</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">杩囨湡鏃堕棿</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid #ddd;">鎿嶄綔</th></tr></thead><tbody>';

    for (var i = 0; i < shares.length; i++) {
      var s = shares[i];
      var statusText = s.expired ? '宸茶繃鏈? : (s.one_time ? '涓€娆℃€? : '娲昏穬');
      var statusColor = s.expired ? '#999' : (s.one_time ? '#e67e22' : '#27ae60');
      var downloads = s.max_downloads > 0 ? s.downloads + '/' + s.max_downloads : s.downloads + '/鈭?;
      var expiresLabel = s.expired ? '-' : (s.expires_at ? new Date(s.expires_at).toLocaleString() : '-');

      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid #eee;max-width:200px;overflow:hidden;text-overflow:ellipsis;" title="' + escHtml(s.filename) + '">' + escHtml(s.filename) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;color:' + statusColor + ';">' + statusText + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;">' + downloads + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-size:12px;">' + expiresLabel + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;text-align:center;">';
      if (!s.expired) {
        html += '<button class="btn btn-danger btn-sm share-revoke-btn" data-token="' + escHtml(s.token) + '">鎾ら攢</button>';
      }
      html += '<button class="btn btn-sm btn-secondary share-copy-btn" data-token="' + escHtml(s.token) + '" style="margin-left:4px;">澶嶅埗</button>';
      html += '</td></tr>';
    }
    html += '</tbody></table>';
    body.innerHTML = html;
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">璇锋眰澶辫触: ' + e.message + '</div>';
  }
}

async function revokeShare(token) {
  if (!confirm('纭畾鎾ら攢姝ゅ垎浜摼鎺ワ紵鎾ら攢鍚庨摼鎺ュ皢绔嬪嵆澶辨晥銆?)) return;
  try {
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', '/api/shares/' + token, {}, null);
    } else {
      var resp = await fetch(BASE + '/api/shares/' + token, { method: 'DELETE', headers: headers() });
      if (!resp.ok) {
        var data = await resp.json().catch(function() { return {}; });
        showToast('鎾ら攢澶辫触: ' + (data.message || resp.status), 'error');
        return;
      }
    }
    showToast('鍒嗕韩閾炬帴宸叉挙閿€', 'success');
    refreshShareList();
  } catch (e) { showToast('鎾ら攢澶辫触: ' + e.message, 'error'); }
}

function copyShareLink(token) {
  var url = location.origin + '/s/' + token;
  if (navigator.clipboard) {
    navigator.clipboard.writeText(url).then(function() {
      showToast('閾炬帴宸插鍒跺埌鍓创鏉?, 'success');
    }).catch(function() {
      showToast('澶嶅埗澶辫触', 'error');
    });
  } else {
    showToast(url, 'success');
  }
}

// --- 浜戠涓嬭浇 ---
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
      if (!resp.ok) { body.innerHTML = '<div class="empty-msg">璇锋眰澶辫触: ' + resp.status + '</div>'; return; }
      tasks = await resp.json();
    }
    _cloudTasks = tasks || [];
    if (_cloudTasks.length === 0) {
      body.innerHTML = '<div class="empty-msg">鏆傛棤涓嬭浇浠诲姟</div>';
      return;
    }
    body.innerHTML = buildCloudTaskTableHtml(_cloudTasks);
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">璇锋眰澶辫触: ' + e.message + '</div>';
  }
}

async function chainDownloadCloud() {
  const input = document.getElementById('cloud-url');
  const text = input.value.trim();
  if (!text) { showToast('请输入下载链接', 'warning'); return; }
  const lines = text.split('\n').map(function(l) { return l.trim(); }).filter(function(l) { return l.length > 0; });
  if (lines.length === 0) { showToast('请输入下载链接', 'warning'); return; }
  try {
    const hdrs = headers({ 'Content-Type': 'application/json' });
    input.value = '';
    showToast('提交任务中...', 'info');
    const urls = lines.map(function(url) { return { url: url }; });
    let tasks;
    if (tunnelHexKey) {
      const result = await tunnelRequest('POST', '/api/cloud/download/batch', hdrs, JSON.stringify({ urls: urls }));
      const data = JSON.parse(new TextDecoder().decode(result.body));
      tasks = data.tasks || [];
    } else {
      const resp = await fetch(BASE + '/api/cloud/download/batch', { method: 'POST', headers: hdrs, body: JSON.stringify({ urls: urls }) });
      const data = await resp.json();
      if (!resp.ok) { showToast('提交失败', 'error'); return; }
      tasks = data.tasks || [];
    }
    refreshCloudTasks();
    showToast(tasks.length + ' 个任务已提交', 'success');
    showToast('等待任务完成...', 'info');
    for (var i = 0; i < 300; i++) {
      await new Promise(function(r) { setTimeout(r, 2000); });
      refreshCloudTasks();
      var allDone = true;
      for (var j = 0; j < tasks.length; j++) {
        try {
          var t;
          if (tunnelHexKey) {
            var r = await tunnelRequest('GET', '/api/cloud/tasks/' + tasks[j].id, {}, null);
            t = JSON.parse(new TextDecoder().decode(r.body));
          } else {
            var r = await fetch(BASE + '/api/cloud/tasks/' + tasks[j].id, { headers: headers() });
            t = await r.json();
          }
          tasks[j] = t;
          if (t.status === 'pending' || t.status === 'downloading') { allDone = false; }
        } catch(e) {}
      }
      if (allDone) { break; }
    }
    var succeeded = tasks.filter(function(t) { return t.status === 'completed'; });
    if (succeeded.length === 0) { showToast('所有任务均未成功完成', 'error'); return; }
    showToast('打包归档中...', 'info');
    var taskIds = succeeded.map(function(t) { return t.id; });
    var archiveHdrs = headers({ 'Content-Type': 'application/json' });
    var archiveResult;
    if (tunnelHexKey) {
      var r = await tunnelRequest('POST', '/api/cloud/archive', archiveHdrs, JSON.stringify({ task_ids: taskIds }));
      archiveResult = JSON.parse(new TextDecoder().decode(r.body));
    } else {
      var r = await fetch(BASE + '/api/cloud/archive', { method: 'POST', headers: archiveHdrs, body: JSON.stringify({ task_ids: taskIds }) });
      archiveResult = await r.json();
    }
    if (!archiveResult.success) { showToast('归档失败', 'error'); return; }
    showToast('下载归档中...', 'info');
    if (tunnelHexKey) {
      await tunnelDownloadStream(archiveResult.file);
    } else {
      window.open(BASE + '/download?filename=' + encodeURIComponent(archiveResult.file), '_blank');
    }
    showToast('清理远端文件...', 'info');
    for (var i = 0; i < taskIds.length; i++) {
      if (tunnelHexKey) {
        await tunnelRequest('DELETE', '/api/cloud/tasks/' + taskIds[i], {}, null);
      } else {
        await fetch(BASE + '/api/cloud/tasks/' + taskIds[i], { method: 'DELETE', headers: headers() });
      }
    }
    refreshCloudTasks();
    showToast('链式下载完成!', 'success');
  } catch (e) { showToast('链式下载失败: ' + e.message, 'error'); }
}
async function createCloudTask() {
  const input = document.getElementById('cloud-url');
  const text = input.value.trim();
  if (!text) { showToast('璇疯緭鍏ヤ笅杞介摼鎺?, 'warning'); return; }

  const lines = text.split('\n').map(function(l) { return l.trim(); }).filter(function(l) { return l.length > 0; });
  if (lines.length === 0) { showToast('璇疯緭鍏ヤ笅杞介摼鎺?, 'warning'); return; }

  try {
    const hdrs = headers({ 'Content-Type': 'application/json' });
    input.value = '';

    if (lines.length === 1) {
      // 鍗?URL锛氫娇鐢ㄥ師鏈?API
      let task;
      if (tunnelHexKey) {
        const result = await tunnelRequest('POST', '/api/cloud/download', hdrs, JSON.stringify({ url: lines[0] }));
        task = JSON.parse(new TextDecoder().decode(result.body));
      } else {
        const resp = await fetch(BASE + '/api/cloud/download', { method: 'POST', headers: hdrs, body: JSON.stringify({ url: lines[0] }) });
        task = await resp.json();
        if (!resp.ok) { showToast('鍒涘缓澶辫触: ' + (task.error || resp.status), 'error'); return; }
      }
      showToast('浠诲姟宸插垱寤? ' + task.id, 'success');
    } else {
      // 澶?URL锛氫娇鐢ㄦ壒閲?API
      const urls = lines.map(function(url) { return { url: url }; });
      let data;
      if (tunnelHexKey) {
        const result = await tunnelRequest('POST', '/api/cloud/download/batch', hdrs, JSON.stringify({ urls: urls }));
        data = JSON.parse(new TextDecoder().decode(result.body));
      } else {
        const resp = await fetch(BASE + '/api/cloud/download/batch', { method: 'POST', headers: hdrs, body: JSON.stringify({ urls: urls }) });
        data = await resp.json();
        if (!resp.ok) { showToast('鍒涘缓澶辫触: ' + (data.error || resp.status), 'error'); return; }
      }
      const tasks = data.tasks || [];
      const failed = tasks.filter(function(t) { return t.status === 'failed'; });
      const succeeded = tasks.filter(function(t) { return t.status !== 'failed'; });
      if (failed.length > 0) {
        showToast(succeeded.length + ' 涓换鍔″凡鍒涘缓, ' + failed.length + ' 涓け璐?, 'warning');
      } else {
        showToast(tasks.length + ' 涓换鍔″凡鍒涘缓', 'success');
      }
    }
    refreshCloudTasks();
  } catch (e) { showToast('鍒涘缓澶辫触: ' + e.message, 'error'); }
}

async function downloadCloudFile(taskId, filename, checksum) {
  try {
    // 鍏堜笅杞戒簯绔枃浠?    const cloudPath = '.__cloud__/' + taskId + '/' + filename;
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
      if (!resp.ok) { showToast('涓嬭浇澶辫触: HTTP ' + resp.status, 'error'); return; }
      buffer = await resp.arrayBuffer();
      serverCS = resp.headers.get('X-File-Checksum') || checksum;
    }

    // 鏍￠獙 checksum
    if (serverCS) {
      const sha256 = new Sha256();
      sha256.update(new Uint8Array(buffer));
      const localCS = sha256.digest();
      if (localCS !== serverCS) {
        showToast('鏍￠獙澶辫触: ' + filename, 'error');
        return;
      }
    }

    // 瑙﹀彂娴忚鍣ㄤ笅杞?    const blob = new Blob([buffer], { type: 'application/octet-stream' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    a.click();
    URL.revokeObjectURL(a.href);
    showToast('涓嬭浇瀹屾垚: ' + filename, 'success');

    // 娓呯悊浜戠鍓湰
    await deleteCloudTask(taskId, filename, serverCS);
  } catch (e) { showToast('涓嬭浇澶辫触: ' + e.message, 'error'); }
}

async function deleteCloudTask(taskId, filename, checksum) {
  try {
    // 鍒犻櫎浜戠鏂囦欢
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
  } catch (e) { /* 闈欓粯澶勭悊 */ }
}

async function cancelCloudTask(taskId) {
  try {
    const url = '/api/cloud/tasks/' + taskId + '/cancel';
    if (tunnelHexKey) {
      await tunnelRequest('POST', url, {}, null);
    } else {
      await fetch(BASE + url, { method: 'POST', headers: headers() });
    }
    showToast('浠诲姟宸插彇娑?, 'success');
    refreshCloudTasks();
  } catch (e) { showToast('鍙栨秷澶辫触: ' + e.message, 'error'); }
}

async function removeCloudTask(taskId) {
  try {
    const url = '/api/cloud/tasks/' + taskId;
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', url, {}, null);
    } else {
      await fetch(BASE + url, { method: 'DELETE', headers: headers() });
    }
    showToast('浠诲姟宸插垹闄?, 'success');
    refreshCloudTasks();
  } catch (e) { showToast('鍒犻櫎澶辫触: ' + e.message, 'error'); }
}

function buildCloudTaskTableHtml(tasks) {
  let html = '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">鏂囦欢鍚?/th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">鐘舵€?/th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">澶у皬</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">鎿嶄綔</th></tr></thead><tbody>';
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
    case 'pending': return '鈴?绛夊緟涓?;
    case 'downloading': return '猬?涓嬭浇涓?;
    case 'completed': return '鉁?宸插畬鎴?;
    case 'failed': return '鉂?澶辫触';
    case 'cancelled': return '馃毇 宸插彇娑?;
    default: return status;
  }
}

function cloudTaskActions(id, filename, status, checksum) {
  let actions = '';
  if (status === 'completed') {
    actions += '<button class="btn btn-primary btn-sm cloud-download-btn" data-id="' + escHtml(id) + '" data-filename="' + escHtml(filename) + '" data-checksum="' + escHtml(checksum || '') + '" style="margin-right:4px;">涓嬭浇鍒版湰鍦?/button>';
    actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">鍒犻櫎</button>';
  } else if (status === 'failed' || status === 'cancelled') {
    actions += '<button class="btn btn-danger btn-sm cloud-remove-btn" data-id="' + escHtml(id) + '">鍒犻櫎</button>';
  } else {
    actions += '<button class="btn btn-warning btn-sm cloud-cancel-btn" data-id="' + escHtml(id) + '">鍙栨秷</button>';
  }
  return actions;
}

// --- 鏂囦欢鐗堟湰绠＄悊 ---
function showVersioning() {
  document.getElementById('version-modal').style.display = 'flex';
  document.getElementById('version-filename').value = '';
  document.getElementById('version-body').innerHTML = '<div class="empty-msg">杈撳叆鏂囦欢鍚嶆煡鐪嬬増鏈巻鍙?/div>';
}

function hideVersioning() {
  document.getElementById('version-modal').style.display = 'none';
}

async function loadVersions() {
  var filename = document.getElementById('version-filename').value.trim();
  if (!filename) { showToast('璇疯緭鍏ユ枃浠跺悕', 'warning'); return; }
  var body = document.getElementById('version-body');
  body.innerHTML = '<div class="empty-msg">鍔犺浇涓?..</div>';
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
        if (resp.status === 501) { body.innerHTML = '<div class="empty-msg">鐗堟湰绠＄悊鏈惎鐢紙闇€鍦ㄩ厤缃腑璁剧疆 versioning.enabled: true锛?/div>'; return; }
        body.innerHTML = '<div class="empty-msg">鍔犺浇澶辫触: ' + (errData.message || resp.status) + '</div>'; return;
      }
      var data = await resp.json();
      versions = data.versions || [];
    }
    if (versions.length === 0) { body.innerHTML = '<div class="empty-msg">璇ユ枃浠舵病鏈夌増鏈巻鍙?/div>'; return; }
    body.innerHTML = buildVersionTableHtml(versions, filename);
  } catch (e) { body.innerHTML = '<div class="empty-msg">鍔犺浇澶辫触: ' + e.message + '</div>'; }
}

function buildVersionTableHtml(versions, filename) {
  var html = '<div style="margin-bottom:8px;font-size:13px;color:#666;">鏂囦欢: <strong>' + escHtml(filename) + '</strong>锛屽叡 ' + versions.length + ' 涓増鏈?/div>';
  html += '<table style="width:100%;border-collapse:collapse;font-size:13px;"><thead><tr>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">鐗堟湰 ID</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">鏃堕棿</th>' +
    '<th style="text-align:left;padding:4px 8px;border-bottom:1px solid #eee;">澶у皬</th>' +
    '<th style="text-align:right;padding:4px 8px;border-bottom:1px solid #eee;">鎿嶄綔</th></tr></thead><tbody>';
  for (var i = 0; i < versions.length; i++) {
    var v = versions[i];
    var versionTime = v.created_at || '-';
    html += '<tr><td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;font-family:monospace;font-size:12px;">' + escHtml(String(v.version_id || '-')) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' + escHtml(versionTime) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;white-space:nowrap;">' + formatSize(v.size) + '</td>' +
      '<td style="padding:6px 8px;border-bottom:1px solid #f0f0f0;text-align:right;white-space:nowrap;">' +
      '<button class="btn btn-primary btn-sm version-restore-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '" style="margin-right:4px;">鎭㈠</button>' +
      '<button class="btn btn-danger btn-sm version-delete-btn" data-filename="' + escHtml(filename) + '" data-version-id="' + escHtml(v.version_id) + '">鍒犻櫎</button></td></tr>';
  }
  html += '</tbody></table>';
  return html;
}

async function restoreVersion(filename, versionId) {
  if (!confirm('纭鎭㈠鐗堟湰 ' + versionId + ' 锛焅n褰撳墠鏂囦欢灏嗚澶囦唤涓烘柊鐗堟湰銆?)) return;
  try {
    var url = '/api/versions/restore?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(versionId);
    if (tunnelHexKey) {
      var result = await tunnelRequest('POST', url, {}, null);
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('鐗堟湰鎭㈠鎴愬姛', 'success'); loadVersions(); refreshList(); }
      else { showToast('鎭㈠澶辫触: ' + (data.message || 'unknown'), 'error'); }
    } else {
      var resp = await fetch(BASE + url, { method: 'POST', headers: headers() });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('鐗堟湰鎭㈠鎴愬姛', 'success'); loadVersions(); refreshList(); }
      else { showToast('鎭㈠澶辫触: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('鎭㈠澶辫触: ' + e.message, 'error'); }
}

async function deleteVersion(filename, versionId) {
  if (!confirm('纭鍒犻櫎鐗堟湰 ' + versionId + ' 锛焅n姝ゆ搷浣滀笉鍙仮澶嶃€?)) return;
  try {
    var url = '/api/versions?filename=' + encodeURIComponent(filename) + '&version_id=' + encodeURIComponent(versionId);
    if (tunnelHexKey) {
      var result = await tunnelRequest('DELETE', url, {}, null);
      var data = JSON.parse(new TextDecoder().decode(result.body));
      if (data.success) { showToast('鐗堟湰宸插垹闄?, 'success'); loadVersions(); }
      else { showToast('鍒犻櫎澶辫触: ' + (data.message || 'unknown'), 'error'); }
    } else {
      var resp = await fetch(BASE + url, { method: 'DELETE', headers: headers() });
      var data = await resp.json();
      if (resp.ok && data.success) { showToast('鐗堟湰宸插垹闄?, 'success'); loadVersions(); }
      else { showToast('鍒犻櫎澶辫触: ' + (data.message || resp.status), 'error'); }
    }
  } catch (e) { showToast('鍒犻櫎澶辫触: ' + e.message, 'error'); }
}

// --- DOMContentLoaded 鍒濆鍖栵細鐢?addEventListener 缁戝畾鎵€鏈夐潤鎬?HTML 鍏冪礌 ---
document.addEventListener('DOMContentLoaded', function() {
  // 璁よ瘉鏍?  document.getElementById('save-token-btn').addEventListener('click', saveToken);
  document.getElementById('save-tunnel-key-btn').addEventListener('click', saveTunnelKey);

  // 鏂囦欢杈撳叆
  document.getElementById('file-input').addEventListener('change', function() {
    uploadFiles(this.files);
  });

  // 宸ュ叿鏍?  document.getElementById('refresh-btn').addEventListener('click', refreshList);
  document.getElementById('search-input').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') searchFiles();
  });
  document.getElementById('search-btn').addEventListener('click', searchFiles);
  document.getElementById('clear-search-btn').addEventListener('click', clearSearch);
  document.getElementById('stats-btn').addEventListener('click', showStats);
  document.getElementById('cloud-btn').addEventListener('click', showCloudDownload);
  document.getElementById('version-btn').addEventListener('click', showVersioning);
  document.getElementById('theme-toggle-btn').addEventListener('click', toggleTheme);

  // 鎵归噺鎿嶄綔
  document.getElementById('batch-delete-btn').addEventListener('click', batchDelete);
  document.getElementById('batch-rename-btn').addEventListener('click', batchRename);
  document.getElementById('batch-archive-btn').addEventListener('click', batchDownloadArchive);
  document.getElementById('batch-clear-btn').addEventListener('click', clearSelection);

  // 鐩綍鎿嶄綔
  document.getElementById('mkdir-btn').addEventListener('click', mkdirDir);

  // 鐩戞帶寮圭獥
  document.getElementById('stats-close-btn').addEventListener('click', hideStats);
  document.getElementById('stats-refresh-btn').addEventListener('click', showStats);
  document.getElementById('stats-close-modal-btn').addEventListener('click', hideStats);
  // 鐩戞帶寮圭獥鏍囩椤靛垏鎹?  document.getElementById('stats-tab').addEventListener('click', function() { switchStatsTab('stats'); });
  document.getElementById('config-tab').addEventListener('click', function() { switchStatsTab('config'); });
  document.getElementById('hub-tab').addEventListener('click', function() { switchStatsTab('hub'); });

  // 浜戠涓嬭浇寮圭獥
  document.getElementById('cloud-close-btn').addEventListener('click', hideCloudDownload);
  document.getElementById('cloud-url').addEventListener('keydown', function(e) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); createCloudTask(); }
  });
  document.getElementById('cloud-chain-btn').addEventListener('click', chainDownloadCloud);
document.getElementById('cloud-submit-btn').addEventListener('click', createCloudTask);
  document.getElementById('cloud-refresh-btn').addEventListener('click', refreshCloudTasks);
  document.getElementById('cloud-close-modal-btn').addEventListener('click', hideCloudDownload);

  // 鐗堟湰绠＄悊寮圭獥
  document.getElementById('version-close-btn').addEventListener('click', hideVersioning);
  document.getElementById('version-filename').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') loadVersions();
  });
  document.getElementById('version-load-btn').addEventListener('click', loadVersions);
  document.getElementById('version-close-modal-btn').addEventListener('click', hideVersioning);

  // 鍒嗕韩寮圭獥浜嬩欢缁戝畾
  document.getElementById('share-close-btn').addEventListener('click', hideShareModal);
  document.getElementById('share-create-tab').addEventListener('click', function() { switchShareTab('create'); });
  document.getElementById('share-list-tab').addEventListener('click', function() { switchShareTab('list'); });
  document.getElementById('share-create-btn').addEventListener('click', createShare);
  document.getElementById('share-list-refresh-btn').addEventListener('click', refreshShareList);

  // 浜嬩欢濮旀墭锛氬姩鎬佸唴瀹?  initDynamicEventDelegation();

  // 鎷栨嫿涓婁紶鍒濆鍖?  initDragAndDrop();
});

// --- 浜嬩欢濮旀墭锛氬姩鎬佺敓鎴愮殑 HTML 鍐呭 ---
function initDynamicEventDelegation() {
  // 鏂囦欢鍒楄〃鍐呯殑鍔ㄦ€佸唴瀹?  const fileList = document.getElementById('file-list');
  if (fileList) {
    // 鏂囦欢琛屼腑鐨勬搷浣滄寜閽?    fileList.addEventListener('click', function(e) {
      const btn = e.target.closest('button');
      if (!btn) return;

      // 鏂囦欢鎿嶄綔鎸夐挳
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

      // 鐩綍鎿嶄綔鎸夐挳锛堥渶瑕侀樆姝㈠啋娉″埌琛岀偣鍑讳簨浠讹級
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

      // 鍔犺浇鏇村鎸夐挳
      if (btn.closest('#load-more-container')) {
        loadMore();
        return;
      }
    });

    // 鐩綍琛岀偣鍑伙紙瀵艰埅鍒扮洰褰曪級
    fileList.addEventListener('click', function(e) {
      const dirRow = e.target.closest('.dir-row');
      if (dirRow && !e.target.closest('button')) {
        navigateDir(dirRow.dataset.subdir);
      }
    });

    // 鍏ㄩ€夊閫夋
    fileList.addEventListener('change', function(e) {
      if (e.target.id === 'select-all-checkbox') {
        toggleSelectAll(e.target.checked);
      }
    });

    // 鍗曚釜鏂囦欢閫夋嫨澶嶉€夋
    fileList.addEventListener('change', function(e) {
      if (e.target.classList.contains('file-select')) {
        updateBatchToolbar();
      }
    });
  }

  // checksum 鐐瑰嚮澶嶅埗
  document.addEventListener('click', function(e) {
    const cell = e.target.closest('.checksum-cell');
    if (cell) {
      copyChecksum(cell.dataset.checksum);
    }
  });

  // 浜戠涓嬭浇浠诲姟鎿嶄綔
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

  // 鐗堟湰绠＄悊鎿嶄綔
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

  // 鍒嗕韩鍒楄〃鎿嶄綔锛堜簨浠跺鎵橈級
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

  // 閰嶇疆闈㈡澘鏇存柊鎸夐挳锛堜簨浠跺鎵橈級
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

  // Hub 闈㈡澘浜嬩欢濮旀墭锛堢Щ闄よ妭鐐规寜閽級
  const hubPanel = document.getElementById('hub-panel');
  if (hubPanel) {
    hubPanel.addEventListener('click', function(e) {
      if (e.target.classList.contains('hub-remove-btn')) {
        removeHubNode(e.target.getAttribute('data-node-id'));
      }
    });
  }
}

// 闈㈠寘灞戜簨浠跺鎵?document.addEventListener('DOMContentLoaded', function() {
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

// --- 鎷栨嫿涓婁紶 ---
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

// --- 鏂囦欢棰勮 ---
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
      if (!resp.ok) { showToast('棰勮澶辫触: ' + resp.status, 'error'); return; }
      text = await resp.text();
    }
    var lines = text.split('\n');
    if (lines.length > 100) {
      text = lines.slice(0, 100).join('\n') + '\n\n... (鍏?' + lines.length + ' 琛岋紝浠呮樉绀哄墠 100 琛?';
    }
    showTextPreview(filename, text);
  } catch (e) { showToast('棰勮澶辫触: ' + e.message, 'error'); }
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


