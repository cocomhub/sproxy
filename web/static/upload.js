// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 上传 UI 模块（app 层）：进度条 / 会话 / 断点续传 DOM。上传纯逻辑委托给
// sclient/api/files.js 的 sc.files.upload（内部自己看 transport 隧道/直连与分块决策）。
// 依赖 sclient/sha256.js, sclient/*, app-render.js（appRender：formatSize/escHtml）、app.js（showToast）。
// 加载顺序（index.html）：upload.js 在 app-render.js 之后——函数体执行时 appRender 已可用；
// 严禁在此文件内新开 formatSize/escHtml 等函数（与 app-render 重复，已因转发删除踩过坑）。
// global（调用点运行期才解引用，加载序安全）：
//   currentSubdir（app.js），showToast（app.js），refreshList（app.js）——
//   Node 测试 require 本文件时这些全局未必存在，须用 typeof 守卫。
//   sclientTransport（sclient/transport.js）——checkResumableUploads 探测 /upload/status 用。
//   transferStore（transfer-store.js）——会话改存 TransferItem（kind:'upload'）+ FS 文件句柄库。
//
// 渲染隔离原则（全部 app 层共同遵守）：
//   1. 纯计算函数（progressText 等）不碰 DOM、单测可直测；
//   2. DOM 写入唯一入口 renderProgress / createProgressBar / showResumePrompt，
//      其它函数不得直接 getElementById 改进度；
//   3. 逻辑（进度回调数据判断）与渲染（progressText→renderProgress）分离。
//   4. Node 单测可 require 本文件（module.exports；DOM/localStorage 仅在运行时访问，
//      顶部不引用 document，事件绑定用 typeof document 守卫）。
//   5. 存储经 setTransferStore 注入（测试传内存 mock；浏览器 defaultStore 惰性单例）。

// ---- 纯计算：进度文案（不碰 DOM，全部边界单测覆盖） ----

// progressText 把进度回调统一成 {pct, text, titleText(可选)}。
// input:{label, loaded, total, totalChunks(可选), chunkIndex(可选), titleText(可选)}
//   - total 缺省/为 0 → pct=0（不 NaN）；loaded 非 number → 0；
//   - totalChunks>1 → 附加「分块 i/N」（chunkIndex 缺省从 1 计，i 封顶 N）；
//   - 否则仅「N%（loaded/total）」。
// 所有数值经 Math.round；不抛错（undefined → 0 文案）。
// （纯函数可测入口与语义亦在 appRender.uploadProgressText；本处为 DOM 无关的本地实现。）
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

// ---- 上传会话持久化（经 transferStore TransferItem kind:'upload'） ----
// 无过渡期：直接使用 sproxy_transfer_items（transfer-store 内管理）；旧键
// sproxy_upload_sessions 不读、不迁移（spec 分节 2）。

// _injectedStore：由 app.js 在页面初始化时调用 upload.setStore(getTransferStore()) 注入
// （index.html 顺序：upload.js 在 app.js 之前，故不能引用 app.js 的 getTransferStore——
// 通过显式 set 注入避免跨文件隐式全局与顶层 let 重名冲突历史（见 app.js/upload.js
// `_transferStore` 声明冲突修复）。Node 测试沿用 setTransferStore 注入内存 mock。
let _injectedStore = null;

function noopStore() {
  const items = [];
  return {
    loadItems: function () { return items; },
    upsertItem: function (item) { const i = items.findIndex(function (it) { return it.id === item.id; }); if (i >= 0) items[i] = item; else items.push(item); },
    removeItem: function (id) { const i = items.findIndex(function (it) { return it.id === id; }); if (i >= 0) items.splice(i, 1); },
    saveFileHandle: function () { return Promise.resolve(); },
    getFileHandle: function () { return Promise.resolve(null); },
    queryFileHandlePermission: function () { return Promise.resolve(null); },
  };
}
function _fallbackStore() {
  return noopStore();
}
// setTransferStore 注入，currentStore() 返回注入 store；无注入 → noopStore（测试/无浏览器环境）。
function setTransferStore(store) { _injectedStore = store || null; }
function currentStore() { return _injectedStore || noopStore(); }
function transferStoreSingleton() { return currentStore(); }
function resetTransferStoreCache() { _injectedStore = null; }

// legacyUploadModuleName：历史地址导出名 upload 恒指向本模块（页内函数声明），
// app.js 经 `setTransferStore(getTransferStore())` 注入——见 app.js 注释。
// 注意：本文件**不得**再声明 getTransferStore（app.js 已声明），否则同页顶层重名。

// completedBitmap(totalChunks)：全 0 块位图（data 为 0/1 数组）。completedChunks
// 数组位扩容（越界/非法索引忽略）。缺 total 或 非数组 → 全 0。
function completedBitmap(completedChunks, totalChunks) {
  const total = Math.max(0, Math.floor(Number(totalChunks) || 0));
  const bitmap = [];
  for (let i = 0; i < total; i++) bitmap[i] = 0;
  if (!Array.isArray(completedChunks)) return bitmap;
  for (const idx of completedChunks) {
    const n = Math.floor(Number(idx));
    if (isFinite(n) && n >= 0 && n < total) bitmap[n] = 1;
  }
  return bitmap;
}

// onSession 会话包（files.js persist 钩子 data）→ kind:'upload' TransferItem。
// meta 存 files.js 会话全部关键字段（uploadId/fileChecksum/totalChunks/chunkSize/
// serverChunkSize/chunksBitmap）；loaded 直接落 item.loaded（展示用）。
function sessionToTransferItem(sess) {
  sess = (sess && typeof sess === 'object') ? sess : {};
  const uploadId = (typeof sess.upload_id === 'string' && sess.upload_id) ? sess.upload_id : (sess.upload_id !== undefined ? String(sess.upload_id) : '');
  const totalSize = (typeof sess.totalSize === 'number' && sess.totalSize > 0) ? sess.totalSize : 0;
  return {
    id: uploadId,
    kind: 'upload',
    filename: (typeof sess.filename === 'string') ? sess.filename : '',
    status: (typeof sess.status === 'string' && sess.status) ? sess.status : 'uploading',
    loaded: (typeof sess.loaded === 'number' && sess.loaded >= 0) ? sess.loaded : 0,
    totalSize: totalSize,
    total: totalSize,
    meta: {
      uploadId: uploadId,
      fileChecksum: (typeof sess.fileChecksum === 'string') ? sess.fileChecksum : '',
      mtimeNano: (typeof sess.mtimeNano === 'number' && isFinite(sess.mtimeNano)) ? sess.mtimeNano : '',
      totalChunks: (typeof sess.totalChunks === 'number' && sess.totalChunks > 0) ? sess.totalChunks : 0,
      chunkSize: (typeof sess.chunkSize === 'number' && sess.chunkSize > 0) ? sess.chunkSize : (typeof sess.meta_serverChunkSize === 'number' && sess.meta_serverChunkSize > 0 ? sess.meta_serverChunkSize : 0),
      serverChunkSize: (typeof sess.serverChunkSize === 'number' && sess.serverChunkSize > 0) ? sess.serverChunkSize : (typeof sess.chunkSize === 'number' && sess.chunkSize > 0 ? sess.chunkSize : 0),
      chunksBitmap: completedBitmap(sess.completedChunks, sess.totalChunks),
    },
  };
}

// saveUploadSession→store.upsertItem（kind:'upload'）：files.js onSession 钩子的 data。
function saveUploadSession(uploadId, data) {
  const item = sessionToTransferItem(Object.assign({}, data, { upload_id: uploadId }));
  item.id = uploadId;
  item.meta.uploadId = uploadId;
  const store = currentStore();
  if (store && typeof store.upsertItem === 'function') store.upsertItem(item);
}

// removeUploadSession→store.removeItem（id=uploadId）。
function removeUploadSession(uploadId) {
  const store = currentStore();
  if (store && typeof store.removeItem === 'function') store.removeItem(uploadId);
}

// resumedChunkCount 供续传提示展示：item.meta.chunksBitmap 置位合计，缺省 0。
// （续传提示展示的是「本机已上传块」——local bitmap；真实缺失由服务端 missing_chunks 权威。）
function resumedChunkCount(item) {
  const bm = (item && item.meta && item.meta.chunksBitmap) || null;
  if (!Array.isArray(bm)) return 0;
  let n = 0;
  for (let i = 0; i < bm.length; i++) if (bm[i]) n++;
  return n;
}

// saveFileHandleForSession：调用方已拿到 granted 的 fileHandle 时把句柄存 sproxy-up-dev
// （键=uploadId）。句柄为空 / 写入失败容忍。
function saveFileHandleForSession(uploadId, fileHandle) {
  if (!uploadId || !fileHandle) return Promise.resolve();
  const store = currentStore();
  if (!store || typeof store.saveFileHandle !== 'function') return Promise.resolve();
  return Promise.resolve(store.saveFileHandle(uploadId, fileHandle)).catch(function () { /* 尽力而为 */ });
}

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

// refreshList：委托 app.js 全局（Node require 时可能不存在，守卫）。本文件不得定义
// 同名函数遮蔽 app.js 的 refreshList（index.html 加载序 upload.js 在 app.js 之后）。
function safeRefreshList() {
  if (typeof refreshList !== 'function') return;
  refreshList();
}

// 分块上传主入口：委托 sc.files.upload（进度条经 onProgress 回调接入）。
// resumeItem 为已持久化的续传 TransferItem（含 meta.uploadId/fileChecksum）。
async function chunkedUpload(file, resumeItem) {
  void resumeItem;
  const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
  const totalSize = file.size || 0;
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
      // 断点续传：把分块会话持久化到 transferStore item（files.js persist 钩子 data=
      // {upload_id,filename,totalSize,totalChunks,status,fileChecksum,loaded,completedChunks…}）。
      // 第二个参数 true = remove（合并成功或『已存在』后清理）。
      onSession: function(sess, remove) {
        if (!sess) return;
        if (remove || sess.upload_id === 'already_exists') { removeUploadSession(sess.upload_id); return; }
        saveUploadSession(sess.upload_id, sess);
      },
    });
    if (result && result.success) {
      // 分块会话的清除由 files.js 上传完成回调 onSession(true) 负责，此处不清——
      // 否则刷新后的 refreshList 会把进行中的大文件分块会话误清，导致断点续传提示消失。
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
      // 简单上传走 POST /upload 单请求：无分块会话（只有分块路径才产生 upload_id 会话），
      // 无需 removeUploadSession。
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

// takeFileHandle：单文件选择成功后尝试拿 FS Access 句柄（showOpenFilePicker 不可用 → null）。
//   showOpenFilePicker({multiple:false, excludeAcceptAllOption:true})——起点目录无法精确指定
//   （浏览器限制），让用户选中目标文件即可；拿到 handle 后 queryPermission('read')，
//   granted → 调用方 saveFileHandleForSession(uploadId, handle) 落库（键=upload_id，
//   upload_id 在 onSession 首次 persist 时才知道）。
// 当前实现：input change 已选好 file 且受浏览器授权读取，故不额外弹 picker；免重选靠
//   resumeUpload(uploadId) 句柄路径。任何失败/取消一律 return null，不 throw（不阻断上传）；
// 多文件 input 不做（无法把选中文件各自与【会话语义无关的】FS 句柄可靠配对）。
async function takeFileHandle(file) {
  try {
    if (typeof window === 'undefined') return null;
    if (typeof window.showOpenFilePicker !== 'function') return null;
    if (!file || typeof file.name !== 'string') return null;
    // showOpenFilePicker 需要再次用户手势（input change 之后自动调用被浏览器禁止）且在弹窗
    // 后用户必须手动重选文件——与『选中即上传』的现有体验相悖，故统一走 input 授权路径。
    // 保留函数签名与注释契约：若未来改为主动 picker 拿句柄，此处实现即可。
    return null;
  } catch (e) {
    return null;
  }
}

// ---- 续传检测 ----

// itemForUploadId：从 store 主列表找 kind:'upload' 且 meta.uploadId===uploadId 的 item
// （id===uploadId 等价——本模块 saveUploadSession 以 upload_id 直接作 item.id；双保险）。
function itemForUploadId(uploadId) {
  const store = currentStore();
  if (!store || typeof store.loadItems !== 'function') return null;
  const all = store.loadItems();
  for (let i = 0; i < all.length; i++) {
    const it = all[i];
    if (it && it.kind === 'upload' && (it.id === uploadId || (it.meta && it.meta.uploadId === uploadId))) return it;
  }
  return null;
}

async function checkResumableUploads() {
  const store = currentStore();
  const uploads = (store && typeof store.loadItems === 'function')
    ? store.loadItems().filter(function (it) { return it && it.kind === 'upload'; })
    : [];
  const results = await Promise.all(uploads.map(function (item) { return statusProbe(item); }));
  const hasResumable = results.some(function (r) { return r === true; });
  if (!hasResumable) {
    const el = document.getElementById('resume-container');
    if (el) el.style.display = 'none';
  }
}

// statusProbe：单个 upload item 的 /upload/status 探测。返回 true 表示应显示续传提示。
//   - hashing → 直接保留+提示（服务端尚无 init 会话，status.success=false 正常）——用户在
//     计算阶段刷新后重选同文件续传仍走全量分块重传（服务端缺失由 init chunk 补全）。
//   - 否则按 probe 结果分流：success.finished → 删；missing_chunks 非空 → 提示；
//     其它（success=false 或 missing 空/非 success）→ 删（服务端会话失联/已完成）。
// probe 请求失败 → 删（会话不可探测默认失联，避免永远挂起）。
function statusProbe(item) {
  const hashing = !!(item && item.status === 'hashing');
  const statusUrl = '/upload/status?upload_id=' + encodeURIComponent(item.id) +
    '&filename=' + encodeURIComponent((item && item.filename) || '');
  return sclientTransport.coreRequest('GET', statusUrl, {}).then(function (result) {
    let status = null;
    try { status = JSON.parse(new TextDecoder().decode(result.body)); } catch (e) { status = null; }
    if (hashing) { showResumePrompt(item, item.id); return true; }
    if (status && status.success && status.finished) {
      removeUploadSession(item.id);
      return false;
    }
    if (status && status.success && status.missing_chunks && status.missing_chunks.length > 0) {
      showResumePrompt(item, item.id);
      return true;
    }
    removeUploadSession(item.id);
    return false;
  }).catch(function () {
    // probe 失败（网络/解析）：服务端会话无法探测 → 默认失联清理（与旧行为一致），
    // 避免残留 item 永远阻塞同文件再上传。
    removeUploadSession(item.id);
    return false;
  });
}

function showResumePrompt(data, uploadId) {
  const el = document.getElementById('resume-container');
  if (!el) return;
  el.style.display = 'block';
  const div = document.createElement('div');
  const done = resumedChunkCount(data);
  const totalChunks = (data && data.meta && data.meta.totalChunks) || (data && data.totalChunks) || 0;
  const title = (data && data.filename) ? data.filename : ((data && data.meta && data.meta.uploadId) ? data.meta.uploadId : '');
  div.style.cssText = 'padding:8px 12px;background:var(--bg-batch);border-radius:4px;margin-bottom:4px;display:flex;align-items:center;gap:8px;flex-wrap:wrap;';
  div.innerHTML =
    '<span style="flex:1;">📦 未完成的上传: <strong>' + appRender.escHtml(title) + '</strong> (' + done + '/' + totalChunks + ' 分块)</span>' +
    '<input type="file" id="resume-file-' + uploadId + '" style="display:none" data-upload-id="' + uploadId + '">' +
    '<button class="resume-btn" data-upload-id="' + uploadId + '">选择文件续传</button>' +
    '<button class="btn btn-sm btn-secondary dismiss-btn" data-upload-id="' + uploadId + '">忽略</button>';
  el.appendChild(div);
}

// hideResumePrompt：移除该 uploadId 的提示行；容器内无任何提示则隐藏整个容器。
function hideResumePrompt(uploadId) {
  const el = document.getElementById('resume-container');
  if (!el) return;
  const promptDiv = el.querySelector('[data-upload-id="' + uploadId + '"]') ? el.querySelector('[data-upload-id="' + uploadId + '"]').closest('div') : null;
  if (promptDiv) promptDiv.remove();
  if (!el.querySelector('.resume-btn')) el.style.display = 'none';
}

function dismissResume(uploadId) {
  removeUploadSession(uploadId);
  const el = document.getElementById('resume-container');
  if (el) el.innerHTML = '';
  checkResumableUploads();
}

// resumeUpload(uploadId, file?)：优先 FS 句柄免重选续传；句柄不可用回落「选择文件续传」。
//   - file 显式传入（选择文件回落）→ 大小校验 + chunkedUpload。
//   - file 缺省（提示按钮/句柄路径）→ getFileHandle(uploadId) → queryPermission('read')
//     → getFile() → file.size===item.totalSize → chunkedUpload 只补缺失块（files.js 内核
//     经 /upload/status 拿 missing_chunks 补传）。句柄缺失/授权拒绝/无 FS API → toast
//     提示走「选择文件续传」按钮（手动回落）。
async function resumeUpload(uploadId, file) {
  const item = itemForUploadId(uploadId);
  if (!item) { showToast('续传数据已丢失', 'error'); return; }
  const savedMtimeNano = (item.meta && isFinite(Number(item.meta.mtimeNano)) && Number(item.meta.mtimeNano) > 0) ? Number(item.meta.mtimeNano) : '';
  // 续传校验：size 必须匹配；mtimeNano 双方可得时也必须匹配（防内容异动续出新旧混合文件）。
  function mtimeMismatch(candidateMtimeNano) {
    return savedMtimeNano !== '' && Number(candidateMtimeNano) !== savedMtimeNano;
  }
  if (file) {
    if (item.totalSize && file.size !== item.totalSize) { showToast('文件大小不匹配，无法续传', 'error'); return; }
    const fileMtimeNano = ((file.lastModified) || Date.now()) * 1000000;
    if (mtimeMismatch(fileMtimeNano)) { showToast('文件已变更，无法续传', 'error'); return; }
    hideResumePrompt(uploadId);
    await chunkedUpload(file, item);
    checkResumableUploads();
    safeRefreshList();
    return;
  }
  // 免重选：FS 句柄 → read 授权 → getFile → size/mtime 校验 → 补缺失块。
  let handle = null;
  try {
    const store = currentStore();
    handle = (store && typeof store.getFileHandle === 'function') ? await store.getFileHandle(uploadId) : null;
  } catch (e) { handle = null; }
  if (!handle) { hideResumePrompt(uploadId); showToast('文件句柄不可用，请选择文件续传', 'info'); return; }
  const perm = (typeof handle.queryPermission === 'function')
    ? await handle.queryPermission({ mode: 'read' }).catch(function () { return 'denied'; })
    : 'denied';
  if (perm !== 'granted') { hideResumePrompt(uploadId); showToast('文件句柄授权被拒绝，请选择文件续传', 'info'); return; }
  let picked = null;
  try { picked = await handle.getFile(); } catch (e) { picked = null; }
  if (!picked) { hideResumePrompt(uploadId); showToast('句柄失效，请选择文件续传', 'info'); return; }
  if (item.totalSize && picked.size !== item.totalSize) { hideResumePrompt(uploadId); showToast('文件已变更，请选择文件续传', 'info'); return; }
  const pickedMtimeNano = ((picked.lastModified) || Date.now()) * 1000000;
  if (mtimeMismatch(pickedMtimeNano)) { hideResumePrompt(uploadId); showToast('文件已变更，请选择文件续传', 'info'); return; }
  hideResumePrompt(uploadId);
  await chunkedUpload(picked, item);
  checkResumableUploads();
  safeRefreshList();
}

// --- 上传入口 ---
// cancelledUploads：按 upload_id 记 per-upload 真暂停标志（暂停按钮置 true）。
// 每批上传开始时统一清空：与新的会话（可能同 upload_id）解耦，避免上次暂停标志
// 泄漏到下一批同 id 上传（无意义残留会误停——上传本来就在跑）。
// 事件委托（app.js）经标记函数操作它，仅在本文件内实现，避免重复实现回归（E-C-1）。
let cancelledUploads = {};
function isCancelledFor(uploadId) { return !!(cancelledUploads[uploadId] || 0); }
function setCancelledUpload(uploadId, flag) {
  if (uploadId === undefined || uploadId === null) return;
  if (flag) cancelledUploads[uploadId] = (cancelledUploads[uploadId] || 0) + 1;
  else delete cancelledUploads[uploadId];
}
// pauseUploadSession：真暂停当前上传——置 per-upload 取消标志（让分块 for 检查点中断）
// + 把本地持久化会话写回 status:'paused'——checkResumableUploads 探 readiness 后正常
// 显示续传提示（探出 missing→提示走既有 resume 路径补缺失块）。不发网络请求；
// statusProbe 对 paused 按普通状态分流（与 uploading 一致，见 statusProbe 注释）。
function pauseUploadSession(item) {
  const uploadId = (item && item.meta && item.meta.uploadId) ? item.meta.uploadId : (item && item.id) || '';
  if (!uploadId) return;
  setCancelledUpload(uploadId, true);
  // 写回 paused（基于最近持久化 data 重建——saveUploadSession 的 upsert 语义自动覆盖旧项）。
  saveUploadSession(uploadId, Object.assign({}, item.meta || {}, {
    filename: item.filename, totalSize: item.totalSize, chunksBitmap: (item.meta && item.meta.chunksBitmap) || [],
    status: 'paused', loaded: item.loaded,
  }));
}

// clearCancelledUpload：取消按钮一键清理（真删会话 + 清 cancel 标志），供事件委托调用。
// 注意：暂停与取消分离——取消保留 removeUploadSession(id)（本函数只是它的便捷增强）。
function clearCancelledUpload(uploadId) {
  setCancelledUpload(uploadId, false);
}

async function uploadFiles(files) {
  if (!files || files.length === 0) return;
  // 每批上传开始清空 per-upload 暂停标志：与上一批的会话解耦（见 cancelledUploads 注释）。
  cancelledUploads = {};
  for (let i = 0; i < files.length; i++) {
    const file = files[i];
    const fileName = currentSubdir ? currentSubdir + '/' + file.name : file.name;
    const size = file.size;
    const progId = createProgressBar(fileName, size, 1);
    // FS Access 免重选：句柄由 resumeUpload(uploadId) 句柄路径承担（无需此处采集）；
    // 不留采集钩子——input change 后无法自动弹 picker 且会破坏『选中即上传』体验。
    // 当前 upload 的会话 id（onSession 首次持久化时捕获）：暂停检查点按它找取消标志。
    let sessUploadId = null;
    const cancelProbe = function () { return !!sessUploadId && isCancelledFor(sessUploadId); };
    try {
      const result = await sc.files.upload(file, {
        subdir: currentSubdir ? currentSubdir : undefined,
        // 真暂停检查点：分块 for 循环每块开头查询本 upload_id 的暂停标志。
        // 暂停按钮（app.js 委托）置标志 → isCancelled 为真 → 抛 E_CANCELLED → 下面的
        // catch 归一为「已暂停」toast；取消按钮则直接 removeUploadSession（走失败路径不重试）。
        isCancelled: cancelProbe,
        onSession: function(sess, remove) {
          // 记录会话 id 供暂停检查点寻址；同一会话持续 persist（含 status/paused 写回）始终覆写。
          if (sess && sess.upload_id) sessUploadId = sess.upload_id;
          if (remove || sess.upload_id === 'already_exists') { removeUploadSession(sess.upload_id); return; }
          saveUploadSession(sess.upload_id, sess);
        },
        onProgress: function(pr) {
          // 分块回调对对象 {loaded,total,chunkIndex,totalChunks}；计算期数值。
          // 统一经 progressText 计算 + renderProgress 渲染（两段隔离）。
          const render = (pr && typeof pr === 'object' && typeof pr.loaded === 'number')
            ? progressText({ label: '上传中…', loaded: pr.loaded, total: pr.total, totalChunks: pr.totalChunks, chunkIndex: pr.chunkIndex, titleText: fileName + ' (' + appRender.formatSize(size) + ', ' + pr.totalChunks + ' 分块)' })
            : progressText({ label: '计算 SHA-256…', loaded: pr || 0, total: size });
          renderProgress(progId, render);
        },
      });
      if (result && result.success) {
        // 分块会话清理由 files.js onSession(true) 负责，此处不误清（见 chunkedUpload 注释）。
        showToast(fileName + ' 上传成功', 'success');
      } else if (result && result.upload_id === 'already_exists') {
        showToast(fileName + ' 已存在，跳过', 'success');
      } else {
        showToast(fileName + ' 上传失败: ' + ((result && result.message) || 'unknown'), 'error');
      }
    } catch (e) {
      if (e && e.code === 'E_CANCELLED') {
        // 真暂停：session 已在上方 pauseUploadSession 写回 paused；此处只提示 + 探续传。
        showToast(fileName + ' 已暂停', 'info');
        checkResumableUploads();
        removeProgressBar(progId);
        continue;
      }
      console.error('[upload] 上传异常', e);
      showToast(fileName + ' 上传失败: ' + e.message, 'error');
    }
    removeProgressBar(progId);
  }
  safeRefreshList();
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
    setTransferStore, transferStoreSingleton, resetTransferStoreCache, currentStore,
    saveUploadSession, removeUploadSession, saveFileHandleForSession,
    sessionToTransferItem, completedBitmap, resumedChunkCount,
    progressText, renderProgress, createProgressBar, removeProgressBar,
    chunkedUpload, simpleUpload, uploadFiles,
    checkResumableUploads, statusProbe, showResumePrompt, hideResumePrompt, dismissResume, resumeUpload,
    takeFileHandle, itemForUploadId,
    isCancelledFor, setCancelledUpload, pauseUploadSession, clearCancelledUpload,
  };
}
