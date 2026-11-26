// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 控制台「卷详情」抽屉：GET /api/volumes/{name}/detail + PUT /api/volumes/{name}/settings/{section}。
//
// 设计要点（docs/designs/2026-11-26-secretdata-block-policy-rules.md §9）：
//   - **分区由后端 settings[] 驱动**：前端不含任何卷型专有分支——后续卷型新增配置只实现后端
//     能力，抽屉自动渲染；未识别的分区退化为只读 JSON（可见、不静默丢弃）。
//   - 唯一有专用渲染器的分区是 block_policy（规则表），因为它需要可编辑表格与本地校验；
//     校验判据复用 volume-settings.js 的纯函数（与后端同一套常量）。
'use strict';

(function () {
  var api = typeof window === 'undefined' ? null : window.v2api;
  var fmt = typeof window === 'undefined' ? null : window.sharedFormat;

  function el(id) { return typeof document === 'undefined' ? null : document.getElementById(id); }
  function settings() { return typeof window === 'undefined' ? null : window.v2volumeSettings; }

  var state = { name: '', sections: [], rows: [] };

  function banner(text, kind) {
    var box = el('v2-vd-state');
    if (!box) { return; }
    box.hidden = !text;
    box.className = 'banner' + (kind === 'warn' ? ' banner-warn' : '');
    box.textContent = text || '';
  }

  function openDrawer(name) {
    state.name = name;
    var t = el('v2-vd-title'); if (t) { t.textContent = '卷详情 · ' + name; }
    var d = el('v2-vdrawer'); var b = el('v2-vd-backdrop');
    if (d) { d.classList.add('show'); d.setAttribute('aria-hidden', 'false'); }
    if (b) { b.hidden = false; }
  }

  function closeDrawer() {
    var d = el('v2-vdrawer'); var b = el('v2-vd-backdrop');
    if (d) { d.classList.remove('show'); d.setAttribute('aria-hidden', 'true'); }
    if (b) { b.hidden = true; }
  }

  /** 打开某卷详情（先开抽屉再拉数据，避免点击无反馈）。 */
  function openDetail(name) {
    if (!api) { return Promise.resolve(); }
    openDrawer(name);
    banner('');
    var info = el('v2-vd-info'); if (info) { info.textContent = '加载中…'; }
    return api.json('GET', '/api/volumes/' + encodeURIComponent(name) + '/detail')
      .then(function (body) { renderDetail(body || {}); })
      .catch(function (e) {
        renderDetail({});
        banner('读取卷详情失败：' + (e.message || e), 'warn');
      });
  }

  function renderDetail(body) {
    var vol = body.volume || {};
    var info = el('v2-vd-info');
    if (info) {
      info.textContent = '';
      var size = fmt ? fmt.formatSize : function (n) { return String(n) + ' B'; };
      [
        ['名称', vol.name || state.name],
        ['类型', vol.type || '—'],
        ['模式', vol.mode || '—'],
        ['容量', Number(vol.capacity) > 0 ? size(vol.capacity) : '不限'],
        ['已用', size(Number(vol.usage) || 0)],
        ['状态', vol.state || '—']
      ].forEach(function (p) {
        var dt = document.createElement('dt'); dt.textContent = p[0];
        var dd = document.createElement('dd'); dd.textContent = p[1];
        info.appendChild(dt); info.appendChild(dd);
      });
    }
    state.sections = Array.isArray(body.settings) ? body.settings : [];
    renderSections();
  }

  function renderSections() {
    var box = el('v2-vd-sections');
    var empty = el('v2-vd-empty');
    if (!box) { return; }
    box.textContent = '';
    if (empty) { empty.hidden = state.sections.length > 0; }
    state.sections.forEach(function (sec) {
      if (sec && sec.key === 'block_policy') { renderBlockPolicy(sec, box); return; }
      box.appendChild(readOnlyCard(sec || {}));
    });
  }

  /** 未识别分区：只读 JSON 卡片（后续卷型新增分区时至少可见）。 */
  function readOnlyCard(sec) {
    var card = document.createElement('div');
    card.className = 'card';
    card.style.marginTop = '12px';
    var h = document.createElement('div');
    h.className = 'card-title';
    h.textContent = sec.label || sec.key || '设置';
    card.appendChild(h);
    var pre = document.createElement('pre');
    pre.className = 'json';
    pre.textContent = JSON.stringify(sec.value || {}, null, 2);
    card.appendChild(pre);
    return card;
  }

  function renderBlockPolicy(sec, box) {
    var S = settings();
    var card = document.createElement('div');
    card.className = 'card';
    card.style.marginTop = '12px';

    var title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = (sec.label || '分块规则') + '（v' + ((sec.value && sec.value.version) || 0) + '）';
    card.appendChild(title);

    var note = document.createElement('div');
    note.className = 'muted';
    note.style.fontSize = '11.5px';
    note.textContent = '按「路径正则 + 体积区间」选择块级模式：single = 整文件单块（1 个底层 blob，需给 max_size）；' +
      'random = 卷默认变长分块。规则只影响新写入的文件，不会重写已存在文件。';
    card.appendChild(note);

    if (sec.read_only) {
      var ro = document.createElement('div');
      ro.className = 'banner banner-warn';
      ro.textContent = '该卷未装配设置持久化后端（state_store），规则只读。';
      card.appendChild(ro);
    }

    var table = document.createElement('table');
    table.className = 'table';
    // 表头用 DOM API 构造（不拼 HTML 字符串）：内容恒为静态文案，但避免 innerHTML 用法。
    var thead = document.createElement('thead');
    var headRow = document.createElement('tr');
    [['路径正则', '36%'], ['min_size', '13%'], ['max_size', '13%'],
      ['模式', '11%'], ['优先级', '9%'], ['', '']].forEach(function (h) {
      var th = document.createElement('th');
      th.textContent = h[0];
      if (h[1]) { th.style.width = h[1]; }
      headRow.appendChild(th);
    });
    thead.appendChild(headRow);
    table.appendChild(thead);
    var tbody = document.createElement('tbody');
    table.appendChild(tbody);
    card.appendChild(table);

    state.rows = S ? S.fromValue(sec.value) : [];
    state.rows = S ? S.sortRules(state.rows) : state.rows;
    fillRuleRows(tbody, !!sec.read_only);

    var bar = document.createElement('div');
    bar.className = 'toolbar';
    bar.style.marginTop = '8px';

    var add = document.createElement('button');
    add.type = 'button';
    add.className = 'btn btn-ghost btn-sm';
    add.id = 'v2-vd-add';
    add.textContent = '新增规则';
    add.disabled = !!sec.read_only;
    add.addEventListener('click', function () {
      if (!S) { return; }
      state.rows = S.addRule(state.rows);
      fillRuleRows(tbody, !!sec.read_only);
    });
    bar.appendChild(add);

    var save = document.createElement('button');
    save.type = 'button';
    save.className = 'btn btn-primary btn-sm';
    save.id = 'v2-vd-save';
    save.textContent = '保存';
    save.disabled = !!sec.read_only;
    save.addEventListener('click', function () { saveBlockPolicy(sec); });
    bar.appendChild(save);
    card.appendChild(bar);

    var errs = document.createElement('div');
    errs.className = 'banner banner-warn';
    errs.id = 'v2-vd-rule-errors';
    errs.hidden = true;
    card.appendChild(errs);

    box.appendChild(card);
  }

  function fillRuleRows(tbody, readOnly) {
    var S = settings();
    var fmtSize = function (n) {
      var num = Number(n) || 0;
      return num > 0 && S ? S.formatSize(num) : '';
    };
    tbody.textContent = '';
    if (state.rows.length === 0) {
      var tr0 = document.createElement('tr');
      var td0 = document.createElement('td');
      td0.colSpan = 6;
      td0.className = 'muted';
      td0.textContent = '暂无规则（未命中任何规则的文件使用卷默认分块策略）';
      tr0.appendChild(td0);
      tbody.appendChild(tr0);
      return;
    }
    state.rows.forEach(function (row, i) {
      var tr = document.createElement('tr');
      tr.appendChild(cell(input('text', row.path_regex, readOnly, function (v) { row.path_regex = v; }), 'mono'));
      tr.appendChild(cell(input('text', fmtSize(row.min_size), readOnly, function (v) { row.min_size = v; })));
      tr.appendChild(cell(input('text', fmtSize(row.max_size), readOnly, function (v) { row.max_size = v; })));
      tr.appendChild(modeCell(row, readOnly));
      tr.appendChild(cell(input('number', String(row.priority || 0), readOnly, function (v) { row.priority = Number(v) || 0; })));
      var act = document.createElement('td');
      act.className = 'r';
      if (!readOnly) {
        var del = document.createElement('button');
        del.type = 'button';
        del.className = 'btn btn-ghost btn-sm';
        del.textContent = '删除';
        del.addEventListener('click', function () {
          if (!settings()) { return; }
          state.rows = settings().removeRule(state.rows, i);
          fillRuleRows(tbody, readOnly);
        });
        act.appendChild(del);
      }
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
  }

  function modeCell(row, readOnly) {
    var td = document.createElement('td');
    var sel = document.createElement('select');
    ['single', 'random'].forEach(function (m) {
      var o = document.createElement('option');
      o.value = m;
      o.textContent = m;
      if (row.mode === m) { o.selected = true; }
      sel.appendChild(o);
    });
    sel.disabled = !!readOnly;
    sel.addEventListener('change', function () { row.mode = sel.value; });
    td.appendChild(sel);
    return td;
  }

  function cell(node, cls) {
    var td = document.createElement('td');
    if (cls) { td.className = cls; }
    td.appendChild(node);
    return td;
  }

  function input(type, value, readOnly, onChange) {
    var n = document.createElement('input');
    n.type = type;
    n.value = value === null || value === undefined ? '' : String(value);
    n.disabled = !!readOnly;
    n.addEventListener('input', function () { onChange(n.value); });
    return n;
  }

  function saveBlockPolicy(sec) {
    var S = settings();
    if (!S || !api) { return Promise.resolve(); }
    var check = S.validateRules(state.rows, { hardMax: S.HARD_MAX_SINGLE_BYTES });
    var box = el('v2-vd-rule-errors');
    if (!check.ok) {
      if (box) {
        box.hidden = false;
        box.textContent = check.errors.map(function (e) {
          return (e.index >= 0 ? '第 ' + (e.index + 1) + ' 条：' : '') + e.msg;
        }).join('；');
      }
      return Promise.resolve();
    }
    if (box) { box.hidden = true; }
    var payload = new TextEncoder().encode(JSON.stringify(S.toPayload(S.sortRules(state.rows))));
    return api.json('PUT', '/api/volumes/' + encodeURIComponent(state.name) +
      '/settings/' + encodeURIComponent(sec.key), { body: payload, contentType: 'application/json' })
      .then(function () {
        if (window.v2toast) { window.v2toast('ok', '分块规则已保存'); }
        return openDetail(state.name);
      })
      .catch(function (e) {
        var msg = (e && e.message) || String(e);
        if (/409/.test(msg)) { msg = '规则已被他人修改，请刷新后重试（并发写冲突）'; }
        banner('保存失败：' + msg, 'warn');
      });
  }

  function init() {
    var c = el('v2-vd-close');
    if (c) { c.addEventListener('click', closeDrawer); }
    var b = el('v2-vd-backdrop');
    if (b) { b.addEventListener('click', closeDrawer); }
  }

  var mod = {
    init: init, open: openDetail, close: closeDrawer,
    renderDetail: renderDetail, renderSections: renderSections,
    renderBlockPolicy: renderBlockPolicy, fillRuleRows: fillRuleRows,
    saveBlockPolicy: saveBlockPolicy
  };
  if (typeof window !== 'undefined') { window.v2volumeDrawer = mod; }
})();
