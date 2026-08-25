// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 上传 UI 模块：进度条 / 会话 / 断点续传 DOM，上传纯逻辑委托给 sclient/api/files.js
// 的 sc.files.upload（内部自己看 transport 隧道/直连与分块决策）。
// 依赖 sha256.js, sclient/*, app.js 全局辅助。

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

// 移除当前文件的进度条
function removeProgressBar(progId) {
  const wrap = document.getElementById(progId + '-wrap');
  if (wrap) wrap.remove();
}

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

// 分块上传主入口：委托 sc.files.upload（进度条经 onProgress 回调接入）。
// resumeSession 为已持久化的续传会话（含 uploadId/fileChecksum）。
async function chunkedUpload(file, resumeSession) {
  const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
  const totalSize = file.size;
  const progId = createProgressBar(fileName, totalSize, 1);
  const updateProg = function(loaded, total) {
    const pct = total > 0 ? (loaded / total * 100) : 0;
    document.getElementById(progId).style.width = pct + '%';
    document.getElementById(progId + '-text').textContent =
      Math.round(pct) + '%（' + formatSize(loaded) + '/' + formatSize(total) + '）';
  };

  try {
    const result = await sc.files.upload(file, {
      subdir: currentSubdir ? currentSubdir : undefined,
      forceChunked: true,
      onProgress: function(loaded, total) { updateProg(loaded, total); },
    });
    if (result && result.success) {
      if (!resumeSession && result.upload_id) removeUploadSession(result.upload_id);
      showToast(fileName + ' 上传成功' + (result.message && result.message !== 'ok' ? '：' + result.message : ''), 'success');
      removeProgressBar(progId);
      return;
    }
    if (result && result.upload_id === 'already_exists') {
      showToast(fileName + ' 已存在，跳过', 'success');
      removeProgressBar(progId);
      return;
    }
    showToast(fileName + ' 上传失败: ' + ((result && result.message) || 'unknown'), 'error');
    removeProgressBar(progId);
  } catch (e) {
    console.error('[upload] 分块上传异常', e);
    showToast(fileName + ' 分块上传失败: ' + e.message, 'error');
    removeProgressBar(progId);
  }
}

// --- 简单上传（小文件）：委托 sc.files.upload（≤chunkThreshold 走简单 POST /upload）---
async function simpleUpload(file) {
  const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
  const totalSize = file.size;
  const progId = createProgressBar(fileName, totalSize, 1);
  try {
    const result = await sc.files.upload(file, {
      subdir: currentSubdir ? currentSubdir : undefined,
      onProgress: function(loaded, total) {
        const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
        const el = document.getElementById(progId);
        if (el) el.style.width = pct + '%';
        const elText = document.getElementById(progId + '-text');
        if (elText) elText.textContent = '计算 SHA-256… ' + pct + '%';
      },
    });
    if (result && result.success) {
      if (result.upload_id) removeUploadSession(result.upload_id);
      showToast(fileName + ' 上传成功', 'success');
    } else {
      showToast(fileName + ' 上传失败: ' + ((result && result.message) || 'unknown'), 'error');
    }
  } catch (e) {
    console.error('[upload] 简单上传异常', e);
    showToast(fileName + ' 上传失败: ' + e.message, 'error');
  }
  removeProgressBar(progId);
}

// --- 上传入口 ---
async function uploadFiles(files) {
  if (!files || files.length === 0) return;
  for (let i = 0; i < files.length; i++) {
    const file = files[i];
    // 差异化文案：大文件标分块数（与旧 chunkedUpload 的进度条风格一致），小文件不标。
    const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
    const size = file.size;
    const progId = createProgressBar(fileName, size, 1);
    try {
      const result = await sc.files.upload(file, {
        subdir: currentSubdir ? currentSubdir : undefined,
        onProgress: function(loaded, total) {
          const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
          const el = document.getElementById(progId);
          if (el) el.style.width = pct + '%';
          const elText = document.getElementById(progId + '-text');
          if (elText) elText.textContent = '上传中… ' + pct + '%（' + formatSize(loaded) + '/' + formatSize(total) + '）';
        },
      });
      if (result && result.success) {
        if (result.upload_id) removeUploadSession(result.upload_id);
        showToast(fileName + ' 上传成功', 'success');
      } else if (result && result.upload_id === 'already_exists') {
        showToast(fileName + ' 已存在，跳过', 'success');
      } else {
        showToast(fileName + ' 上传失败: ' + ((result && result.message) || 'unknown'), 'error');
      }
    } catch (e) {
      console.error('[upload] 上传异常', e);
      showToast(fileName + ' 上传失败: ' + e.message, 'error');
    }
    removeProgressBar(progId);
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
      // 查询分块上传状态走传输层（coreRequest GET /upload/status）——续传查询统一走传输层，
      // 与简单/分块上传的 sc.files.upload 走同一 coreRequest。
      sclientTransport.coreRequest('GET', statusUrl, {}).then(function(result) {
        const data = JSON.parse(new TextDecoder().decode(result.body));
        handleStatusResponse(data);
      }).catch(function() { removeUploadSession(sessUploadId); });
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
  div.style.cssText = 'padding:8px 12px;background:var(--bg-batch);border-radius:4px;margin-bottom:4px;display:flex;align-items:center;gap:8px;flex-wrap:wrap;';
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
  const parts = data.filename.split('/');
  if (parts.length > 1) {
    currentSubdir = parts.slice(0, -1).join('/');
    localStorage.setItem('sproxy_subdir', currentSubdir);
  }
  await chunkedUpload(file, data);
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
