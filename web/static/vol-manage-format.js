// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// vol-manage-format.js WebUI「卷管理」面板渲染纯函数（stats-modal「卷管理」tab）。
// 服务端数据源：
//   GET   /api/backends      → {backends:[{type, category, label?, fields:[FieldSchema]}]}
//         FieldSchema{key,label,type,required,options?,allow_wrapper?}（type: text|volume-select|enum|bool|number）
//   GET   /api/volumes/user  → {volumes:[{name,type,capacity,usage?,extra?}]}（owner 过滤）
//   POST  /api/volumes/user  {name,type,capacity,extra}（schema 校验 + 防环；400 错误文案由后端给）
//   DELETE /api/volumes/user?name=<n>（运行中引用 → 409）
// 纯函数可 node --test，不依赖 DOM。卷类型/字段全部由 /api/backends 驱动，前端不硬编码类型。
// 样式统一 var(--…) 禁内联亮色 hex；HTML 转义复用 appRender.escHtml（禁自建 escHtml）。
// global: appRender（app-render.js，转义）
//
// 安全边界：建卷涉及底层卷选择与防环，全部由服务端校验负责（前端仅透传 400/409 错误文案），
// 不在本客户端重复实现校验。

'use strict';

// volManageFormHtml(opts) → schema 驱动建卷表单 HTML 字符串。
// opts: {type, category, label, fields, volumes}。
//   - type/category/label：当前选中后端类型（渲染为 hidden name="type" data-type + 标题）；
//   - fields：该类型的创建表单字段（FieldSchema[]），逐字段渲染控件：
//       volume-select → 底层卷下拉（候选来自 volumes，每项 {name, category}；
//                        field.allow_wrapper=true 才包含 category==='wrapper' 的封装卷）；
//       enum          → <select> 选项来自 field.options；
//       text/number   → 对应 <input type=text|number>；
//       bool          → <input type=checkbox>；
//       required      → 控件带 required 属性（必填由后端校验兜底，前端仅作提示）。
//   - volumes：可选底层卷候选（对象数组；缺省空）。
// 控件 name=field.key；创建时按 name 读表单值组装 extra[key]（见 app.js onSubmitVolManage）。
function volManageFormHtml(opts) {
  const o = opts || {};
  const fields = Array.isArray(o.fields) ? o.fields : [];
  const vols = Array.isArray(o.volumes) ? o.volumes : [];
  const type = o.type || '';
  let m = '<div style="margin-top:12px;padding:12px;border:1px solid var(--border-color);border-radius:6px;">';
  m += '<div style="font-weight:600;margin-bottom:8px;">创建卷' +
    (o.label || type ? '（' + appRender.escHtml(o.label || type) + '）' : '') + '</div>';
  m += '<input type="hidden" name="type" data-type="' + appRender.escHtml(type) + '" value="' + appRender.escHtml(type) + '">';
  m += '<div style="display:flex;flex-direction:column;gap:9px;">';
  m += fieldWrapHtml('卷名（唯一）', false,
    '<input type="text" name="name" placeholder="卷名" style="margin-top:4px;padding:6px 8px;border:1px solid var(--border-input);border-radius:4px;background:var(--bg-input,var(--bg-container));color:var(--text-primary);font-size:13px;">');
  fields.forEach(function (f) {
    m += formFieldHtml(f, vols);
  });
  m += fieldWrapHtml('容量（留空不限，如 100GiB）', false,
    '<input type="text" name="capacity" placeholder="可选" style="margin-top:4px;padding:6px 8px;border:1px solid var(--border-input);border-radius:4px;background:var(--bg-input,var(--bg-container));color:var(--text-primary);font-size:13px;">');
  m += '<div style="display:flex;gap:8px;align-items:center;margin-top:4px;">' +
    '<button type="button" id="vm-create-btn" class="btn btn-sm btn-primary">创建卷</button>' +
    '<span id="vm-msg" style="font-size:12px;color:var(--text-muted);"></span></div>';
  m += '</div></div>';
  return m;
}

// formFieldHtml(f, vols) → 单个 schema 字段的控件 HTML（label + input/select）。
function formFieldHtml(f, vols) {
  const key = appRender.escHtml(f.key);
  const label = f.label || f.key;
  const star = f.required ? ' *' : '';
  const reqAttr = f.required ? ' required' : '';
  const inputStyle = 'margin-top:4px;padding:6px 8px;border:1px solid var(--border-input);border-radius:4px;background:var(--bg-input,var(--bg-container));color:var(--text-primary);font-size:13px;';
  let ctrl;
  if (f.type === 'volume-select') {
    // 底层卷下拉：候选来自 vols，过滤掉封装卷（除非 allow_wrapper）。
    const base = vols.filter(function (v) {
      return f.allow_wrapper || !v || (v.category || '') !== 'wrapper';
    });
    ctrl = '<select name="' + key + '"' + reqAttr + ' style="' + inputStyle + '">';
    ctrl += '<option value="">（选择底层卷）</option>';
    base.forEach(function (v) {
      if (!v || v.name == null) return;
      const nm = appRender.escHtml(String(v.name));
      ctrl += '<option value="' + nm + '">' + nm + '</option>';
    });
    ctrl += '</select>';
    // 嵌套封装：底层卷 + 「新空子目录」→ 提交时拼成 `<卷>/<子目录>`（互斥占用/须不存在由
    // 服务端校验，前端仅回显 409 错误文案）。子目录留空 = 传统整卷语义（仅 local+root 装配）。
    ctrl += '<input type="text" name="' + key + '_subdir" placeholder="嵌套子目录（可选：底层卷新空子目录）"' +
      ' style="margin-top:4px;' + inputStyle + '">';
  } else if (f.type === 'enum') {
    ctrl = '<select name="' + key + '"' + reqAttr + ' style="' + inputStyle + '">';
    (f.options || []).forEach(function (opt) {
      const ov = appRender.escHtml(String(opt == null ? '' : opt));
      ctrl += '<option value="' + ov + '">' + ov + '</option>';
    });
    ctrl += '</select>';
  } else if (f.type === 'bool') {
    ctrl = '<input type="checkbox" name="' + key + '" value="true" style="margin-top:4px;">';
  } else {
    const it = f.type === 'number' ? 'number' : 'text';
    ctrl = '<input type="' + it + '" name="' + key + '"' + reqAttr + ' style="' + inputStyle + '">';
  }
  return fieldWrapHtml(label, star, ctrl);
}

// fieldWrapHtml(labelTxt, star, control) → 单个字段的外层（label + 控件）。
function fieldWrapHtml(labelTxt, star, control) {
  return '<div style="display:flex;flex-direction:column;">' +
    '<label style="font-size:13px;color:var(--text-secondary);">' + appRender.escHtml(labelTxt) +
    (star ? appRender.escHtml(star) : '') + '</label>' + control + '</div>';
}

// volManageListHtml(opts) → 「我的卷」列表表格（名称 / 类型 / category / 容量 / 操作-删除）。
// opts.volumes: [{name, type, category, capacity, usage}]。category 缺失时回落 type。
// 删除按钮 data-delete-volume=<name>（app.js 事件委托；409 → toast「有任务引用」）。
function volManageListHtml(opts) {
  const o = opts || {};
  const vols = o.volumes || [];
  if (!Array.isArray(vols) || vols.length === 0) {
    return '<div class="empty-msg">暂无用户卷，选择类型并提交上方表单创建。</div>';
  }
  let h = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
  h += '<thead><tr style="background:var(--bg-hover);">';
  h += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">卷名</th>';
  h += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">类型</th>';
  h += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">category</th>';
  h += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid var(--border-color);">容量</th>';
  h += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid var(--border-color);">操作</th>';
  h += '</tr></thead><tbody>';
  vols.forEach(function (v) {
    const cat = (v && (v.category || v.type)) || '-';
    const capTxt = v && v.capacity > 0 ? appRender.formatSize(v.capacity) : '不限';
    h += '<tr>';
    h += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-weight:600;">' + appRender.escHtml(v.name) + '</td>';
    h += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;color:var(--text-secondary);">' + appRender.escHtml(v.type || '-') + '</td>';
    h += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);font-size:12px;">' + appRender.escHtml(cat) + '</td>';
    h += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);">' + capTxt + '</td>';
    h += '<td style="padding:6px 8px;border-bottom:1px solid var(--border-color);text-align:center;">' +
      '<button type="button" class="btn btn-sm btn-danger" data-delete-volume="' + appRender.escHtml(v.name) + '" style="padding:2px 8px;font-size:12px;">删除</button></td>';
    h += '</tr>';
  });
  h += '</tbody></table>';
  return h;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    volManageFormHtml: volManageFormHtml,
    volManageListHtml: volManageListHtml,
    composeNestedTarget: composeNestedTarget,
  };
}

// composeNestedTarget(vol, subdir) → 嵌套封装 target 拼装：`<卷>/<子目录>`。
// 子目录为空 → 返回卷名（传统整卷语义）；卷名为空 → 空串（先选底层卷）。
// 前端纯函数（node --test 覆盖；提交时把 volume-select 值与其子目录输入合并）。
function composeNestedTarget(vol, subdir) {
  const v = String(vol == null ? '' : vol).trim();
  const s = String(subdir == null ? '' : subdir).trim();
  if (!s) return v;
  if (!v) return '';
  return v + '/' + s;
}