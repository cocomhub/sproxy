// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 分块上传模块。依赖 sha256.js, tunnel.js, app.js 中的辅助函数。

const SESSIONS_KEY = 'sproxy_upload_sessions';

function loadSessions() {
  try {
    return JSON.parse(localStorage.getItem(SESSIONS_KEY)) || {};
  } catch { return {}; }
}

function saveSessions(sessions) {
  try { localStorage.setItem(SESSIONS_KEY, JSON.stringify(sessions)); } catch { /* ignore */ }
}

function saveUploadSession(uploadId, data) {
  const sessions = loadSessions();
  sessions[uploadId] = data;
  saveSessions(sessions);
}

function removeUploadSession(uploadId) {
  const sessions = loadSessions();
  delete sessions[uploadId];
  saveSessions(sessions);
}

// 流式 SHA-256（文件级），支持进度回调，使用纯 JS 增量实现
// 每次读取 64MB 到内存计算，与预读缓冲区大小一致，减少磁盘寻址
async function computeSHA256(file, onProgress) {
  const sha256 = new Sha256();
  const cs = Math.min(64 * 1024 * 1024, file.size || Infinity);
  const tc = Math.ceil(file.size / cs);
  for (let i = 0; i < tc; i++) {
    const s = i * cs;
    const e = Math.min(s + cs, file.size);
    const buffer = await file.slice(s, e).arrayBuffer();
    sha256.update(new Uint8Array(buffer));
    if (onProgress) onProgress(s + buffer.byteLength, file.size);
  }
  return sha256.digest();
}

// 流式 SHA-256（Blob 级）
async function computeSHA256Blob(blob) {
  const sha256 = new Sha256();
  if (blob.size <= 4 * 1024 * 1024) {
    sha256.update(new Uint8Array(await blob.arrayBuffer()));
    return sha256.digest();
  }
  const cs = Math.min(4 * 1024 * 1024, blob.size);
  const tc = Math.ceil(blob.size / cs);
  for (let i = 0; i < tc; i++) {
    const buf = await blob.slice(i * cs, Math.min((i + 1) * cs, blob.size)).arrayBuffer();
    sha256.update(new Uint8Array(buf));
  }
  return sha256.digest();
}

const BASE_CHUNK_SIZE = 4 * 1024 * 1024;  // 4 MiB
const MAX_CHUNK_SIZE = 64 * 1024 * 1024;  // 64 MiB

function calcChunkSize(fileSize) {
  let chunkSize = BASE_CHUNK_SIZE;
  while (chunkSize * 512 < fileSize && chunkSize < MAX_CHUNK_SIZE) {
    chunkSize *= 2;
  }
  return Math.min(chunkSize, MAX_CHUNK_SIZE);
}

function generateUploadId(filename, totalSize, lastModified, fileChecksum) {
  const mtimeNano = (lastModified || Date.now()) * 1_000_000;
  const raw = filename + '|' + totalSize + '|' + mtimeNano + '|' + fileChecksum;
  const encoder = new TextEncoder();
  return crypto.subtle.digest('SHA-256', encoder.encode(raw)).then(function(hash) {
    return bytesToHex(new Uint8Array(hash)).substring(0, 32);
  });
}

// 预读缓冲区：减少 file.slice().arrayBuffer() 的磁盘寻址次数
// 每次从磁盘读取最多 PRELOAD_SIZE 字节到内存，然后在内存中切片供后续分块使用
// 64 MB 的平衡点：对 160 MB 文件只需 3 次磁盘读取（原来 40 次），内存占用可控
const PRELOAD_SIZE = 64 * 1024 * 1024; // 64 MB

// 创建分块读取器（每个上传会话独立，无并发问题）
// 使用 Generator 模式：每次读取一个 4MB 分块，预读后续 64MB 到缓冲区
function createChunkReader(file) {
  let buf = null;
  let bufStart = 0;
  let bufEnd = 0;

  return {
    async read(start, end) {
      if (buf && start >= bufStart && end <= bufEnd) {
        // 缓冲区命中，直接在内存中切片（slice 拷贝 ~4MB，但避免了磁盘寻址）
        return buf.slice(start - bufStart, end - bufStart);
      }
      // 缓冲区未命中，从磁盘读取 64MB
      const loadEnd = Math.min(start + PRELOAD_SIZE, file.size);
      const raw = await file.slice(start, loadEnd).arrayBuffer();
      buf = new Uint8Array(raw);
      bufStart = start;
      bufEnd = loadEnd;
      return buf.slice(0, end - start);
    },
    release() {
      buf = null;
    },
  };
}

// 移除当前文件的进度条
function removeProgressBar(progId) {
  const wrap = document.getElementById(progId + '-wrap');
  if (wrap) wrap.remove();
}

// 分块上传主函数
async function chunkedUpload(file, tunnelMode, resumeSession) {
  const reader = createChunkReader(file);
  const totalSize = file.size;
  const chunkSize = calcChunkSize(totalSize);
  const totalChunks = Math.ceil(totalSize / chunkSize);
  const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;

  console.log('[upload] 开始上传', { fileName, totalSize, chunkSize, totalChunks, resumeSession: !!resumeSession });

  let uploadId;
  let fileChecksum;

  // 创建进度条
  const progId = createProgressBar(fileName, totalSize, totalChunks);
  const updateProg = function(loaded, total, chunkIdx) {
    const pct = total > 0 ? (loaded / total * 100) : 0;
    document.getElementById(progId).style.width = pct + '%';
    document.getElementById(progId + '-text').textContent =
      Math.round(pct) + '%（分块 ' + (chunkIdx + 1) + '/' + totalChunks + ', ' + formatSize(loaded) + '/' + formatSize(total) + '）';
  };

  if (resumeSession) {
    uploadId = resumeSession.uploadId;
    fileChecksum = resumeSession.fileChecksum;
    console.log('[upload] 续传模式', { uploadId, fileChecksum: fileChecksum ? fileChecksum.substring(0, 16) : null });
    document.getElementById(progId + '-text').textContent = '续传中…';
  } else {
    try {
      console.log('[upload] 开始计算 SHA-256', { fileName, totalSize });
      document.getElementById(progId + '-text').textContent = '计算 SHA-256…';
      fileChecksum = await computeSHA256(file, function(loaded, total) {
        const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
        document.getElementById(progId).style.width = pct + '%';
        document.getElementById(progId + '-text').textContent = '计算 SHA-256… ' + pct + '%（' + formatSize(loaded) + '/' + formatSize(total) + '）';
      });
      console.log('[upload] SHA-256 计算完成', { checksum: fileChecksum.substring(0, 16) });
    } catch (e) {
      console.error('[upload] SHA-256 计算失败', e);
      showToast(fileName + ' SHA-256 计算失败: ' + e.message, 'error');
      const wrap = document.getElementById(progId + '-wrap');
      if (wrap) wrap.remove();
      return;
    }
    uploadId = await generateUploadId(fileName, totalSize, file.lastModified, fileChecksum);
    console.log('[upload] 生成 uploadId', { uploadId });
  }

  try {
    document.getElementById(progId + '-text').textContent = '初始化上传…';
    console.log('[upload] 发送 initUpload', { uploadId, fileName, totalSize, chunkSize, totalChunks });
    const initResult = await initUpload(uploadId, fileName, totalSize, chunkSize, totalChunks, fileChecksum, tunnelMode);
    console.log('[upload] initUpload 响应', initResult);
    if (!initResult.success) {
      showToast(fileName + ' 初始化失败: ' + (initResult.message || 'unknown'), 'error');
      return;
    }
    if (initResult.upload_id === 'already_exists') {
      showToast(fileName + ' 已存在，跳过', 'success');
      return;
    }

    console.log('[upload] 查询缺失分块', { uploadId: initResult.upload_id });
    const missingChunks = await queryMissingChunks(initResult.upload_id);
    console.log('[upload] 缺失分块结果', { missingChunks });
    const actualUploadId = initResult.upload_id;

    // 使用服务端调整后的 chunk_size（如果服务端裁剪了大小）
    const adjustedChunkSize = initResult.chunk_size || chunkSize;
    const adjustedTotalChunks = Math.ceil(totalSize / adjustedChunkSize);
    console.log('[upload] 调整后分块参数', { adjustedChunkSize, adjustedTotalChunks, serverChunkSize: initResult.chunk_size });

    const chunkIndices = buildChunkIndices(adjustedTotalChunks, missingChunks, resumeSession);
    console.log('[upload] 待上传分块索引', chunkIndices);

    // 计算已上传的字节数（续传场景下已有部分分块上传完成）
    let uploadedBytes = 0;
    if (resumeSession && resumeSession.completedChunks) {
      for (const ci of resumeSession.completedChunks) {
        uploadedBytes += adjustedChunkSize;
      }
      if (uploadedBytes > totalSize) uploadedBytes = totalSize;
    }
    // 更新进度条显示已上传的部分
    updateProg(uploadedBytes, totalSize, chunkIndices.length > 0 ? chunkIndices[0] : 0);

    saveUploadSession(actualUploadId, {
      filename: fileName, totalSize: totalSize, chunkSize: adjustedChunkSize,
      totalChunks: adjustedTotalChunks, fileChecksum: fileChecksum,
      lastModified: file.lastModified, uploadId: actualUploadId,
      completedChunks: resumeSession ? (resumeSession.completedChunks || []) : [],
      status: 'uploading', startedAt: Date.now()
    });
    const sessionData = loadSessions()[actualUploadId];
    if (!sessionData.completedChunks) sessionData.completedChunks = [];

    // 串行上传每个分块（带宽瓶颈场景下并发不增加吞吐量，反而增加复杂度）
    for (let ci = 0; ci < chunkIndices.length; ci++) {
      const idx = chunkIndices[ci];
      const start = idx * adjustedChunkSize;
      const end = Math.min(start + adjustedChunkSize, totalSize);
      const chunkBytes = await reader.read(start, end);
      const chunkChecksum = await calcChunkSha256(chunkBytes);

      const ok = await uploadChunkWithRetry(actualUploadId, idx, chunkBytes, chunkChecksum, tunnelMode);
      if (!ok) {
        showToast(fileName + ' 分块 ' + idx + ' 上传失败（重试耗尽）', 'error');
        return;
      }
      uploadedBytes += (end - start);
      updateProg(uploadedBytes, totalSize, idx);
      if (!sessionData.completedChunks.includes(idx)) {
        sessionData.completedChunks.push(idx);
        saveUploadSession(actualUploadId, sessionData);
      }
    }

    console.log('[upload] 发送 completeUpload', { uploadId: actualUploadId });
    const completeResult = await completeUpload(actualUploadId, tunnelMode);
    console.log('[upload] completeUpload 响应', completeResult);
    if (completeResult.success) {
      showToast(fileName + ' 上传成功，校验通过', 'success');
      removeUploadSession(actualUploadId);
      // 移除当前文件的进度条，不影响其他正在上传的进度条
      const wrap = document.getElementById(progId + '-wrap');
      if (wrap) wrap.remove();
    } else {
      sessionData.status = 'failed';
      saveUploadSession(actualUploadId, sessionData);
      showToast(fileName + ' 合并失败: ' + (completeResult.message || 'unknown'), 'error');
      const wrap = document.getElementById(progId + '-wrap');
      if (wrap) wrap.remove();
    }
  } catch (e) {
    console.error('[upload] 分块上传异常', e);
    showToast(fileName + ' 分块上传失败: ' + e.message, 'error');
  }
  reader.release();
}

// --- 辅助函数 ---

let _progCounter = 0;
function createProgressBar(fileName, totalSize, totalChunks) {
  const progId = 'prog-' + Date.now() + '-' + (++_progCounter);
  const container = document.getElementById('upload-progress-container');
  container.insertAdjacentHTML('beforeend',
    '<div id="' + progId + '-wrap"><small>' + escHtml(fileName) + ' (' + formatSize(totalSize) + ', ' + totalChunks + ' 分块)</small>' +
    '<div class="upload-progress"><div class="upload-progress-bar" id="' + progId + '"></div></div>' +
    '<div class="chunk-progress-text" id="' + progId + '-text">等待中…</div></div>');
  return progId;
}

async function calcChunkSha256(chunkBytes) {
  // 使用 Web Crypto API（浏览器原生实现，比纯 JS 快 ~36x）
  const hash = await crypto.subtle.digest('SHA-256', chunkBytes);
  return bytesToHex(new Uint8Array(hash));
}

async function initUpload(uploadId, fileName, totalSize, chunkSize, totalChunks, fileChecksum, tunnelMode) {
  const initBody = {
    upload_id: uploadId, filename: fileName, total_size: totalSize,
    chunk_size: chunkSize, total_chunks: totalChunks, file_checksum: fileChecksum
  };
  if (tunnelMode) {
    const initResp = await tunnelRequest('POST', '/upload/init',
      { 'Content-Type': 'application/json' },
      new TextEncoder().encode(JSON.stringify(initBody)));
    return JSON.parse(new TextDecoder().decode(initResp.body));
  }
  const resp = await fetch(BASE + '/upload/init', {
    method: 'POST',
    headers: headers({ 'Content-Type': 'application/json' }),
    body: JSON.stringify(initBody)
  });
  return resp.json();
}

async function queryMissingChunks(uploadID) {
  const statusResp = await fetch(BASE + '/upload/status?upload_id=' + uploadID, { headers: headers() });
  if (statusResp.ok) {
    const statusData = await statusResp.json();
    if (statusData.success) {
      return statusData.missing_chunks || [];
    }
  }
  return null;
}

function buildChunkIndices(totalChunks, missingChunks, resumeSession) {
  let indices;
  if (Array.isArray(missingChunks)) {
    // 服务端返回了缺失分块列表（可能为空数组 = 全已接收），使用它作为权威列表
    indices = missingChunks;
  } else if (resumeSession && resumeSession.completedChunks) {
    indices = [];
    for (let i = 0; i < totalChunks; i++) {
      if (!resumeSession.completedChunks.includes(i)) indices.push(i);
    }
  } else {
    indices = [];
    for (let i = 0; i < totalChunks; i++) indices.push(i);
  }
  return indices;
}

async function uploadChunkWithRetry(uploadID, idx, chunkBytes, chunkChecksum, tunnelMode) {
  let lastErr = null;
  for (let retry = 0; retry < 3; retry++) {
    try {
      const result = await uploadChunk(uploadID, idx, chunkBytes, chunkChecksum, tunnelMode);
      if (result.success) return true;
      if (!result.should_retry) return false;
    } catch (e) {
      // 网络异常（断网、超时等），记录错误后继续重试
      lastErr = e;
      console.warn('[upload] 分块 ' + idx + ' 上传异常（第 ' + (retry + 1) + ' 次）', e.message);
    }
  }
  if (lastErr) throw lastErr;
  return false;
}

async function uploadChunk(uploadID, idx, chunkBytes, chunkChecksum, tunnelMode) {
  if (tunnelMode) {
    return uploadChunkViaTunnel(uploadID, idx, chunkBytes, chunkChecksum);
  }
  const formData = new FormData();
  formData.append('upload_id', uploadID);
  formData.append('chunk_index', String(idx));
  formData.append('chunk_checksum', chunkChecksum);
  formData.append('chunk', new Blob([chunkBytes]), String(idx).padStart(5, '0') + '.chunk');
  const resp = await fetch(BASE + '/upload/chunk', {
    method: 'POST',
    headers: headers({}),
    body: formData
  });
  return resp.json();
}

async function uploadChunkViaTunnel(uploadID, idx, chunkBytes, chunkChecksum) {
  const boundary = '----WebKitFormBoundary' + crypto.getRandomValues(new Uint32Array(1))[0].toString(36);
  const encoder = new TextEncoder();
  const parts = [
    encoder.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="upload_id"\r\n\r\n' + uploadID + '\r\n'),
    encoder.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="chunk_index"\r\n\r\n' + idx + '\r\n'),
    encoder.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="chunk_checksum"\r\n\r\n' + chunkChecksum + '\r\n'),
    encoder.encode('--' + boundary + '\r\nContent-Disposition: form-data; name="chunk"; filename="' + String(idx).padStart(5, '0') + '.chunk"\r\nContent-Type: application/octet-stream\r\n\r\n'),
    chunkBytes,
    encoder.encode('\r\n--' + boundary + '--\r\n')
  ];
  const tlen = parts.reduce(function(s, p) { return s + p.byteLength; }, 0);
  const fullBody = new Uint8Array(tlen);
  let off = 0;
  for (let pi = 0; pi < parts.length; pi++) { fullBody.set(parts[pi], off); off += parts[pi].byteLength; }
  const treq = await tunnelRequest('POST', '/upload/chunk',
    { 'Content-Type': 'multipart/form-data; boundary=' + boundary }, fullBody);
  return JSON.parse(new TextDecoder().decode(treq.body));
}

async function completeUpload(uploadID, tunnelMode) {
  if (tunnelMode) {
    const cresp = await tunnelRequest('POST', '/upload/complete',
      { 'Content-Type': 'application/json' },
      new TextEncoder().encode(JSON.stringify({ upload_id: uploadID })));
    return JSON.parse(new TextDecoder().decode(cresp.body));
  }
  const resp = await fetch(BASE + '/upload/complete', {
    method: 'POST',
    headers: headers({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ upload_id: uploadID })
  });
  return resp.json();
}

// --- 上传入口 ---
async function uploadFiles(files) {
  if (!files || files.length === 0) return;
  for (let i = 0; i < files.length; i++) {
    await chunkedUpload(files[i], !!tunnelHexKey, null);
  }
  refreshList();
}

// --- 续传检测 ---
function checkResumableUploads() {
  const sessions = loadSessions();
  let hasResumable = false;
  for (const uploadId in sessions) {
    const data = sessions[uploadId];
    if (data.status !== 'uploading') continue;
    hasResumable = true;
    (function(sessionData, sessUploadId) {
      const statusUrl = '/upload/status?upload_id=' + sessUploadId + '&filename=' + encodeURIComponent(sessionData.filename);
      function handleStatusResponse(status) {
        if (status.success && !status.finished && status.missing_chunks && status.missing_chunks.length > 0) {
          showResumePrompt(sessionData, sessUploadId);
        } else if (status.success && status.finished) {
          removeUploadSession(sessUploadId);
        } else {
          removeUploadSession(sessUploadId);
        }
      }
      if (tunnelHexKey) {
        tunnelRequest('GET', statusUrl, {}, null)
          .then(function(result) {
            var data = JSON.parse(new TextDecoder().decode(result.body));
            handleStatusResponse(data);
          })
          .catch(function() { removeUploadSession(sessUploadId); });
      } else {
        fetch(BASE + statusUrl, { headers: headers() })
          .then(function(r) { return r.json(); })
          .then(handleStatusResponse)
          .catch(function() { removeUploadSession(sessUploadId); });
      }
    })(data, uploadId);
  }
  if (!hasResumable) {
    const el = document.getElementById('resume-container');
    if (el) el.style.display = 'none';
  }
}

function showResumePrompt(data, uploadId) {
  const el = document.getElementById('resume-container');
  if (!el) return;
  el.style.display = 'block';
  const div = document.createElement('div');
  div.style.cssText = 'padding:8px 12px;background:#f0fff0;border-radius:4px;margin-bottom:4px;display:flex;align-items:center;gap:8px;flex-wrap:wrap;';
  div.innerHTML = '<span style="flex:1;">📦 未完成的上传: <strong>' + escHtml(data.filename) + '</strong> (' + (data.completedChunks ? data.completedChunks.length : 0) + '/' + data.totalChunks + ' 分块)</span>' +
    '<input type="file" id="resume-file-' + uploadId + '" style="display:none" data-upload-id="' + uploadId + '">' +
    '<button class="resume-btn" data-upload-id="' + uploadId + '">选择文件续传</button>' +
    '<button class="btn btn-sm btn-secondary dismiss-btn" data-upload-id="' + uploadId + '">忽略</button>';
  el.appendChild(div);
}

function dismissResume(uploadId) {
  removeUploadSession(uploadId);
  const el = document.getElementById('resume-container');
  if (el) el.innerHTML = '';
  checkResumableUploads();
}

async function resumeUpload(uploadId, file) {
  if (!file) return;
  const sessions = loadSessions();
  const data = sessions[uploadId];
  if (!data) { showToast('续传数据已丢失', 'error'); return; }
  if (file.size !== data.totalSize) { showToast('文件大小不匹配，无法续传', 'error'); return; }

  // 隐藏当前续传提示，表示已开始处理
  const resumeContainer = document.getElementById('resume-container');
  if (resumeContainer) {
    const promptDiv = resumeContainer.querySelector('[data-upload-id="' + uploadId + '"]')?.closest('div');
    if (promptDiv) promptDiv.remove();
    // 没有更多续传提示时隐藏容器
    if (!resumeContainer.querySelector('.resume-btn')) {
      resumeContainer.style.display = 'none';
    }
  }

  showToast('正在校验文件 SHA-256，请稍候…', 'info');
  try {
    const checksum = await computeSHA256(file, function(loaded, total) {
      const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
      showToast('校验文件 SHA-256… ' + pct + '%', 'info');
    });
    if (checksum !== data.fileChecksum) { showToast('文件内容不匹配（SHA-256 不一致），无法续传', 'error'); return; }
  } catch (e) { showToast('SHA-256 计算失败: ' + e.message, 'error'); return; }
  showToast('文件校验通过，开始续传…', 'success');
  const parts = data.filename.split('/');
  if (parts.length > 1) {
    currentSubdir = parts.slice(0, -1).join('/');
    localStorage.setItem('sproxy_subdir', currentSubdir);
  }
  await chunkedUpload(file, !!tunnelHexKey, data);
  checkResumableUploads();
  refreshList();
}

// --- 续传容器事件委托 ---
document.addEventListener('DOMContentLoaded', function() {
  const resumeContainer = document.getElementById('resume-container');
  if (!resumeContainer) return;

  // 点击"选择文件续传"按钮 → 触发隐藏的 file input
  resumeContainer.addEventListener('click', function(e) {
    const btn = e.target.closest('.resume-btn');
    if (btn) {
      const uploadId = btn.dataset.uploadId;
      const fileInput = document.getElementById('resume-file-' + uploadId);
      if (fileInput) fileInput.click();
      return;
    }
  });

  // 点击"忽略"按钮
  resumeContainer.addEventListener('click', function(e) {
    const btn = e.target.closest('.dismiss-btn');
    if (btn) {
      dismissResume(btn.dataset.uploadId);
      return;
    }
  });

  // 文件选择变化 → 触发续传
  resumeContainer.addEventListener('change', function(e) {
    const fileInput = e.target.closest('input[type="file"]');
    if (fileInput && fileInput.id && fileInput.id.startsWith('resume-file-')) {
      const uploadId = fileInput.dataset.uploadId;
      if (uploadId && fileInput.files && fileInput.files[0]) {
        resumeUpload(uploadId, fileInput.files[0]);
      }
    }
  });
});
