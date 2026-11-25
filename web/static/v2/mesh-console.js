// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台集群分区：Mesh 状态（GET /api/mesh/status）与 Hub 节点（GET /api/hub/nodes|stats）。
// Hub 未启用时服务端返回 403/404 ⇒ 分区展示明确的「未启用」而不是空白。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;

  function el(id) { return document.getElementById(id); }

  function banner(id, text, kind) {
    var box = el(id);
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function row(cells, cls) {
    var tr = document.createElement('tr');
    cells.forEach(function (v, i) {
      var td = document.createElement('td');
      td.textContent = v == null || v === '' ? '—' : String(v);
      if (i === 0) { td.className = 'mono'; }
      tr.appendChild(td);
    });
    if (cls) { tr.className = cls; }
    return tr;
  }

  function faceText(f) {
    if (!f) { return '未启用'; }
    return (f.enabled ? '启用' : '未启用') + (f.addr ? ' · ' + f.addr : '') + ' · pinned ' + (f.pinned || 0);
  }

  function renderMesh(st) {
    var tbody = el('v2-mesh-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    var s = st || {};
    tbody.appendChild(row(['remote_read', faceText(s.remote_read), s.remote_read && s.remote_read.enabled ? 'ok' : 'off']));
    tbody.appendChild(row(['remote_write', faceText(s.remote_write), s.remote_write && s.remote_write.enabled ? 'ok' : 'off']));
    if (s.node) {
      tbody.appendChild(row(['node', (s.node.running ? '运行中' : '未运行') + ' · ' + (s.node.node_id || '—'),
        (s.node.hub_url || '') + (s.node.webrtc ? ' · webrtc' : '')]));
    } else {
      tbody.appendChild(row(['node', '未配置', 'off']));
    }
    tbody.appendChild(row(['signaling', s.signaling_enabled ? '启用' : '未启用', s.hub_url || '—']));
    var empty = el('v2-mesh-empty');
    if (empty) { empty.hidden = true; }
  }

  function loadMesh() {
    if (!api) { return; }
    banner('v2-mesh-state', '');
    api.json('GET', '/api/mesh/status')
      .then(function (st) { renderMesh(st); })
      .catch(function (e) { banner('v2-mesh-state', 'Mesh 状态读取失败：' + (e.message || e), 'warn'); });
  }

  var hubAdmin = false;

  function renderHub(nodes, stats) {
    var tbody = el('v2-hub-body');
    if (!tbody) { return; }
    tbody.textContent = '';
    nodes.forEach(function (n) {
      var tr = row([
        n.id || n.node_id || '', (n.services || []).join(', ') || '—',
        (n.quality || '—'), n.rtt_ms == null ? '—' : (n.rtt_ms < 0 ? '未采样' : n.rtt_ms + ' ms'),
        (n.virtual_ip || '—')
      ]);
      if (hubAdmin) {
        var act = document.createElement('td');
        act.className = 'r actions';
        var id = n.id || n.node_id || '';
        if (id) { act.appendChild(kickBtn(id)); }
        tr.appendChild(act);
      }
      tbody.appendChild(tr);
    });
    var empty = el('v2-hub-empty');
    if (empty) { empty.hidden = nodes.length > 0; }
    var stat = el('v2-hub-stats');
    if (stat) {
      stat.hidden = false;
      stat.textContent = '节点 ' + (stats.nodes != null ? stats.nodes : nodes.length) +
        ' · 在线 ' + (stats.online != null ? stats.online : '—') +
        ' · 转发 ' + (stats.relayed != null ? stats.relayed : '—');
    }
  }

  /** 「踢出」按钮（仅 admin 渲染）：二次确认后 DELETE /api/hub/nodes/{id}。 */
  function kickBtn(id) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn-ghost btn-sm';
    b.textContent = '踢出';
    b.setAttribute('data-kick', id);
    b.addEventListener('click', function () { kickNode(id); });
    return b;
  }

  function kickNode(id) {
    if (!window.confirm('踢出节点 ' + id + '？该节点将从中继路由与 DHT 中移除，需重新注册才能加入。')) { return; }
    api.json('DELETE', '/api/hub/nodes/' + encodeURIComponent(id))
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '已踢出节点 ' + id); }
        loadHub();
      })
      .catch(function (e) {
        banner('v2-hub-state', '踢出失败：' + (e.message || e), 'warn');
      });
  }

  /** 探测当前凭据角色（admin 才显示踢出按钮）；无权限/接口不可用时保持只读。 */
  function probeAdmin() {
    return api.json('GET', '/api/config')
      .then(function () { return api.json('GET', '/api/credentials'); })
      .then(function () { hubAdmin = true; })
      .catch(function () { hubAdmin = false; });
  }

  function loadHub() {
    if (!api) { return; }
    banner('v2-hub-state', '');
    Promise.all([
      api.json('GET', '/api/hub/nodes').catch(function (e) { throw e; }),
      api.json('GET', '/api/hub/stats').catch(function () { return {}; })
    ]).then(function (r) {
      var body = r[0] || {};
      renderHub(body.nodes || [], r[1] || {});
    }).catch(function (e) {
      renderHub([], {});
      banner('v2-hub-state', 'Hub 未启用或不可用：' + (e.message || e), 'warn');
    });
  }

  var inited = false;
  function initMesh() {
    if (!inited) {
      var r = el('v2-mesh-refresh'); if (r) { r.addEventListener('click', loadMesh); }
      inited = true;
    }
    if (el('v2-mesh-body')) { loadMesh(); }
  }

  var hubInited = false;
  function initHub() {
    if (!hubInited) {
      var r = el('v2-hub-refresh'); if (r) { r.addEventListener('click', loadHub); }
      hubInited = true;
    }
    if (el('v2-hub-body')) { probeAdmin().then(loadHub); }
  }

  if (typeof window !== 'undefined') { window.v2mesh = { initMesh: initMesh, initHub: initHub }; }
})();
