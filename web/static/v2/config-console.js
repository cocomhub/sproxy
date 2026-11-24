// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台配置分区：GET /api/config 展示 + 仅提交变更字段到 PUT /api/config。
// 服务端可写字段：log_level / log_format / max_storage_bytes / max_upload_bytes /
// max_file_meta_bytes / rate_limit_requests / rate_limit_window / web_tunnel。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  var EDITABLE = ['log_level', 'log_format', 'max_storage_bytes', 'max_upload_bytes',
    'max_file_meta_bytes', 'rate_limit_requests', 'rate_limit_window', 'web_tunnel'];

  /** 只保留与原始值不同的可写字段（PUT 语义：未指定即不改）。 */
  function diffConfig(orig, next) {
    var out = {};
    EDITABLE.forEach(function (k) {
      if (!(k in next)) { return; }
      var a = orig ? orig[k] : undefined;
      var b = next[k];
      if (typeof b === 'number' && typeof a === 'number') {
        if (a !== b) { out[k] = b; }
        return;
      }
      if (String(a === undefined || a === null ? '' : a) !== String(b === undefined || b === null ? '' : b)) {
        out[k] = b;
      }
    });
    return out;
  }

  /** 字节输入：空/非法/负数 ⇒ null（不提交该字段）。 */
  function parseBytes(text) {
    var t = String(text == null ? '' : text).trim();
    if (!t) { return null; }
    if (!/^\d+$/.test(t)) { return null; }
    var n = Number(t);
    return Number.isSafeInteger(n) && n >= 0 ? n : null;
  }

  function el(id) { return document.getElementById(id); }
  function val(id) { var n = el(id); return n ? n.value : ''; }
  function checked(id) { var n = el(id); return n ? n.checked : false; }

  function banner(text, kind) {
    var box = el('v2-cfg-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  var original = null;

  function fill(cfg) {
    original = cfg;
    var set = function (id, v) { var n = el(id); if (n) { n.value = v == null ? '' : String(v); } };
    set('v2-cfg-log-level', cfg.log_level || 'info');
    set('v2-cfg-log-format', cfg.log_format || 'text');
    set('v2-cfg-max-storage', cfg.max_storage_bytes || 0);
    set('v2-cfg-max-upload', cfg.max_upload_bytes || 0);
    set('v2-cfg-max-meta', cfg.max_file_meta_bytes || 0);
    set('v2-cfg-rl-req', cfg.rate_limit_requests || '');
    set('v2-cfg-rl-win', cfg.rate_limit_window || '');
    var wt = el('v2-cfg-web-tunnel'); if (wt) { wt.checked = !!cfg.web_tunnel; }

    var facts = el('v2-cfg-facts');
    if (facts) {
      facts.textContent = '';
      [
        ['地址', cfg.addr || '—'],
        ['存储根', cfg.storage_root || '—'],
        ['TLS', cfg.tls_enabled ? '启用' : '未启用'],
        ['版本管理', cfg.versioning_enabled ? ('启用（保留 ' + cfg.versioning_max_versions + ' 版）') : '未启用'],
        ['分块大小', fmt.formatSize(cfg.chunk_size || 0)],
        ['上传会话 TTL', cfg.upload_session_ttl || '—'],
        ['云下载并发', String(cfg.cloud_max_concurrent || 0)],
        ['转存并发', String(cfg.cloud_transfer_concurrency || 0)],
        ['同步阈值', fmt.formatSize(cfg.cloud_sync_threshold || 0)],
        ['Hub', cfg.hub_enabled ? '启用' : '未启用'],
        ['AccessKey', cfg.access_keys_set ? '已配置（SproxySig）' : '未配置']
      ].forEach(function (kv) {
        var row = document.createElement('div');
        row.className = 'kv';
        var k = document.createElement('span'); k.className = 'k muted'; k.textContent = kv[0];
        var v = document.createElement('span'); v.className = 'v'; v.textContent = kv[1];
        row.appendChild(k); row.appendChild(v);
        facts.appendChild(row);
      });
    }
    updateHints();
  }

  function updateHints() {
    [['v2-cfg-max-storage', 'v2-cfg-max-storage-hint'],
      ['v2-cfg-max-upload', 'v2-cfg-max-upload-hint'],
      ['v2-cfg-max-meta', 'v2-cfg-max-meta-hint']].forEach(function (p) {
      var hint = el(p[1]);
      if (!hint) { return; }
      var n = parseBytes(val(p[0]));
      hint.textContent = n === null ? '字节（0 = 不限制）' : fmt.formatSize(n);
    });
  }

  function load() {
    banner('');
    api.json('GET', '/api/config')
      .then(function (cfg) { fill(cfg || {}); })
      .catch(function (e) { banner('配置读取失败：' + (e.message || e), 'warn'); });
  }

  function collect() {
    return {
      log_level: val('v2-cfg-log-level'),
      log_format: val('v2-cfg-log-format'),
      max_storage_bytes: parseBytes(val('v2-cfg-max-storage')),
      max_upload_bytes: parseBytes(val('v2-cfg-max-upload')),
      max_file_meta_bytes: parseBytes(val('v2-cfg-max-meta')),
      rate_limit_requests: Number(val('v2-cfg-rl-req')) || 0,
      rate_limit_window: val('v2-cfg-rl-win'),
      web_tunnel: checked('v2-cfg-web-tunnel')
    };
  }

  function save() {
    var diff = diffConfig(original, collect());
    Object.keys(diff).forEach(function (k) { if (diff[k] === null) { delete diff[k]; } });
    if (Object.keys(diff).length === 0) {
      banner('没有需要保存的变更。');
      return;
    }
    var body = new TextEncoder().encode(JSON.stringify(diff));
    api.json('PUT', '/api/config', { body: body, contentType: 'application/json' })
      .then(function () {
        banner('');
        if (window.v2toast) { window.v2toast('ok', '配置已更新'); }
        load();
      })
      .catch(function (e) { banner('保存失败：' + (e.message || e), 'warn'); });
  }

  var inited = false;

  function init() {
    if (inited) { load(); return; }
    inited = true;
    var btn = el('v2-cfg-save'); if (btn) { btn.addEventListener('click', save); }
    var reset = el('v2-cfg-reset'); if (reset) { reset.addEventListener('click', load); }
    ['v2-cfg-max-storage', 'v2-cfg-max-upload', 'v2-cfg-max-meta'].forEach(function (id) {
      var n = el(id); if (n) { n.addEventListener('input', updateHints); }
    });
    if (el('v2-cfg-facts')) { load(); }
  }

  if (typeof window !== 'undefined') { window.v2config = { init: init, load: load, diffConfig: diffConfig, parseBytes: parseBytes }; }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { diffConfig: diffConfig, parseBytes: parseBytes };
  }
})();
