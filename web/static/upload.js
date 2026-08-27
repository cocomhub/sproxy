// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 上传 UI 模块（app 层）：进度条 / 会话 / 断点续传 DOM。上传纯逻辑委托给
// sclient/api/files.js 的 sc.files.upload（内部自己看 transport 隧道/直连与分块决策）。
// 依赖 sclient/sha256.js, sclient/*, app-render.js（appRender：formatSize/escHtml）、app.js（showToast）。
//
// 渲染隔离原则（全部 app 层共同遵守）：
//   1. 纯计算函数（progressText 等）不碰 DOM、单测可直测；
//   2. DOM 写入唯一入口 renderProgress / createProgressBar / showResumePrompt，
//      其它函数不得直接 getElementById 改进度；
//   3. 逻辑（进度回调数据判断）与渲染（progressText→renderProgress）分离。
//   4. Node 单测可 require 本文件（module.exports；DOM/localStorage 仅在运行时访问，
//      顶部不引用 document，事件绑定用 typeof document 守卫）。

const SESSIONS_KEY = 'sproxy_upload_sessions';

// ---- 纯计算：进度文案（不碰 DOM，全部边界单测覆盖） ----

// progressText 把进度回调统一成 {pct, text, titleText(可选)}。
// input:{label, loaded, total, totalChunks(可选), chunkIndex(可选), titleText(可选)}
//   - total 缺省/为 0 → pct=0（不 NaN）；loaded 非 number → 0；
//   - totalChunks>1 → 附加「分块 i/N」（chunkIndex 缺省从 1 计，i 封顶 N）；
//   - 否则仅「N%（loaded/total）」。
// 所有数值经 Math.round；不抛错（undefined → 0 文案）。
function progressText(input) {
  const i = (input && typeof input === 'object') ? input : {};
  const loaded = (typeof i.loaded === 'number' && i.loaded >= 0) ? i.loaded : 0;
  const total = (typeof i.total === 'number' && i.total > 0) ? i.total : 0;
  const pct = total > 0 ? Math.round(loaded / total * 100) : 0;
  const sizeTxt = appRender.formatSize(loaded) + '/' + appRender.formatSize(total);
  const label = (typeof i.label === 'string' && i.label) ? i.label : '上传中…';
  const tc = i.totalChunks;
  let text;
  if (tc && tc > 1) {
    const idx = (typeof i.chunkIndex === 'number') ? i.chunkIndex + 1 : 1;
    text = label + ' ' + pct + '%（' + sizeTxt + '，分块 ' + Math.min(tc, idx) + '/' + tc + '）';
  } else {
    text = label + ' ' + pct + '%（' + sizeTxt + '）';
  }
  const out = { pct: pct, text: text };
  if (typeof i.titleText === 'string') out.titleText = i.titleText;
  return out;
}

// ---- 会话持久化（逻辑部分不碰 DOM） ----

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
  sessions[uploadId] = data || {};
  saveSessions(sessions);
}

function removeUploadSession(uploadId) {
  const sessions = loadSessions();
  delete sessions[uploadId];
  saveSessions(sessions);
}

// resumedChunkCount 供续传提示展示：completedChunks 数组长度，缺省回退 0。
function resumedChunkCount(data) { return (data && Array.isArray(data.completedChunks)) ? data.completedChunks.length : 0; }

// ---- DOM 渲染入口（纯写入，不做计算） ----
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
    '<div id="' + progId + '-wrap"><small>' + appRender.escHtml(fileName) + ' (' + appRender.formatSize(totalSize) + ', ' + totalChunks + ' 分块)</small>' +
    '<div class="upload-progress"><div class="upload-progress-bar" id="' + progId + '"></div></div>' +
    '<div class="chunk-progress-text" id="' + progId + '-text">等待中…</div></div>');
  return progId;
}

// renderProgress 统一渲染：render(pct,text,titleText?) → 进度条宽度 + 文案 + 标题。
// 是 app 层唯一「进度 → DOM」入口；计算一律走 progressText。
function renderProgress(progId, render) {
  if (!progId) return;
  const r = render || {};
  const el = document.getElementById(progId);
  if (el && typeof r.pct === 'number') el.style.width = r.pct + '%';
  const elText = document.getElementById(progId + '-text');
  if (elText && typeof r.text === 'string') elText.textContent = r.text;
  if (r.titleText) {
    const small = document.querySelector('#' + progId + '-wrap small');
    if (small && typeof r.titleText === 'string') small.textContent = r.titleText;
  }
}

// 分块上传主入口：委托 sc.files.upload（进度条经 onProgress 回调接入）。
// resumeSession 为已持久化的续传会话（含 uploadId/fileChecksum）。
async function chunkedUpload(file, resumeSession) {
  const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
  const totalSize = file.size;
  const progId = createProgressBar(fileName, totalSize, 1);
  try {
    const result = await sc.files.upload(file, {
      subdir: currentSubdir ? currentSubdir : undefined,
      forceChunked: true,
      onProgress: function(pr) {
        // 分块回调对对象（{loaded,total,chunkIndex,totalChunks}）；计算期数值。
        // 统一经 progressText 计算 + renderProgress 渲染（两段隔离）。
        const render = (pr && typeof pr === 'object')
          ? progressText({ label: '上传中…', loaded: pr.loaded, total: pr.total, totalChunks: pr.totalChunks, chunkIndex: pr.chunkIndex, titleText: fileName + ' (' + appRender.formatSize(totalSize) + ', ' + pr.totalChunks + ' 分块)' })
          : progressText({ label: '计算 SHA-256…', loaded: pr || 0, total: totalSize });
        renderProgress(progId, render);
      },
      // 断点续传：把分块会话持久化到 localStorage（files.js 每块成功/完成时回调）。
      // 第二个参数 true = remove（合并成功或『已存在』后清理）。
      onSession: function(sess, remove) {
        if (remove || sess.upload_id === 'already_exists') { removeUploadSession(sess.upload_id); return; }
        saveUploadSession(sess.upload_id, sess);
      },
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
      onProgress: function(pr) {
        renderProgress(progId, progressText({ label: '计算 SHA-256…', loaded: pr, total: totalSize }));
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
    const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
    const size = file.size;
    const progId = createProgressBar(fileName, size, 1);
    try {
      const result = await sc.files.upload(file, {
        subdir: currentSubdir ? currentSubdir : undefined,
        onProgress: function(pr) {
          // 分块回调对象 {loaded,total,chunkIndex,totalChunks}；简单回调数值。
          // 统一：total 缺省/0 以 size 兜底（修复历史 totalSize 未定义）。两段隔离。
          const render = (pr && typeof pr === 'object' && typeof pr.loaded === 'number')
            ? progressText({ label: '上传中…', loaded: pr.loaded, total: pr.total, totalChunks: pr.totalChunks, chunkIndex: pr.chunkIndex, titleText: fileName + ' (' + appRender.formatSize(size) + ', ' + pr.totalChunks + ' 分块)' })
            : progressText({ label: '计算 SHA-256…', loaded: pr || 0, total: size });
          renderProgress(progId, render);
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
        if (status.success && status.finished) {
          removeUploadSession(sessUploadId);
        } else if (status.success && status.missing_chunks && status.missing_chunks.length > 0) {
          showResumePrompt(sessionData, sessUploadId);
        } else {
          removeUploadSession(sessUploadId);
        }
      }
      // 查询分块上传状态走传输层（coreRequest GET /upload/status）——续传查询统一走传输层。
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
  const done = resumedChunkCount(data);
  const totalChunks = (data && data.totalChunks) || 0;
  div.style.cssText = 'padding:8px 12px;background:var(--bg-batch);border-radius:4px;margin-bottom:4px;display:flex;align-items:center;gap:8px;flex-wrap:wrap;';
  div.innerHTML = '<span style="flex:1;">📦 未完成的上传: <strong>' + appRender.escHtml((data && data.filename) || '') + '</strong> (' + done + '/' + totalChunks + ' 分块)</span>' +
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

  const resumeContainer = document.getElementById('resume-container');
  if (resumeContainer) {
    const promptDiv = resumeContainer.querySelector('[data-upload-id="' + uploadId + '"]')?.closest('div');
    if (promptDiv) promptDiv.remove();
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

// --- 续传容器事件委托（Node require 时无 document，跳过绑定） ---
if (typeof document !== 'undefined') {
  document.addEventListener('DOMContentLoaded', function() {
    const resumeContainer = document.getElementById('resume-container');
    if (!resumeContainer) return;

    resumeContainer.addEventListener('click', function(e) {
      const btn = e.target.closest('.resume-btn');
      if (btn) {
        const uploadId = btn.dataset.uploadId;
        const fileInput = document.getElementById('resume-file-' + uploadId);
        if (fileInput) fileInput.click();
        return;
      }
    });

    resumeContainer.addEventListener('click', function(e) {
      const btn = e.target.closest('.dismiss-btn');
      if (btn) {
        dismissResume(btn.dataset.uploadId);
        return;
      }
    });

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
}

// ---- Node 单测导出（浏览器全局由函数声明提供，不需该分支） ----
if (typeof module === 'object' && module.exports) {
  module.exports = {
    loadSessions, saveSessions, saveUploadSession, removeUploadSession,
    progressText, renderProgress, createProgressBar, removeProgressBar,
    chunkedUpload, simpleUpload, uploadFiles,
    checkResumableUploads, showResumePrompt, dismissResume, resumeUpload, resumedChunkCount,
  };
}
