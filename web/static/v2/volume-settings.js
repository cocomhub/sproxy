// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 卷设置「分块规则」表的纯函数模块（无 DOM 依赖，可 node --test）。
// 判据与后端 policy.Compile 对齐：single 必须给 max_size、≤ 硬上限(64MiB) 与卷块上限、
// 正则必须可编译、单卷 ≤ 64 条。前端校验只为体验，安全边界仍在后端（服务端二次校验）。
'use strict';

(function () {
  var HARD_MAX_SINGLE_BYTES = 64 * 1024 * 1024;
  var MAX_RULES = 64;
  var MODES = ['single', 'random'];

  /** 报告值是否「未填写」（undefined / null / 纯空白）。 */
  function isBlank(v) {
    return v === undefined || v === null || (typeof v === 'string' && v.trim() === '');
  }

  /** 解析人类可读字节（"20MiB"/"1GB"/纯数字）→ 字节；非法 → null。 */
  function parseSize(text) {
    if (text === null || text === undefined) { return null; }
    if (typeof text === 'number') { return isFinite(text) && text >= 0 ? Math.floor(text) : null; }
    var s = String(text).trim();
    if (s === '') { return null; }
    var m = /^(\d+(?:\.\d+)?)\s*([kmgt]?i?b)?$/i.exec(s);
    if (!m) { return null; }
    var n = parseFloat(m[1]);
    var unit = (m[2] || '').toLowerCase();
    var mult = 1;
    if (unit) {
      var base = unit.indexOf('i') >= 0 ? 1024 : 1000;
      var exp = { k: 1, m: 2, g: 3, t: 4 }[unit.charAt(0)] || 0;
      mult = Math.pow(base, exp);
    }
    return Math.floor(n * mult);
  }

  /** 字节 → 人类可读（与 parseSize 互逆，取整到整数 MiB/KiB）。 */
  function formatSize(bytes) {
    var n = Number(bytes);
    if (!isFinite(n) || n <= 0) { return '0'; }
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n = n / 1024; i++; }
    var rounded = Math.round(n * 100) / 100;
    return String(rounded) + ' ' + units[i];
  }

  /** 按 priority 降序稳定排序（返回新数组；同 priority 保持原序）。 */
  function sortRules(rules) {
    var arr = (rules || []).slice();
    return arr
      .map(function (r, i) { return { r: r, i: i }; })
      .sort(function (a, b) {
        var pa = Number(a.r.priority) || 0;
        var pb = Number(b.r.priority) || 0;
        if (pa !== pb) { return pb - pa; }
        return a.i - b.i;
      })
      .map(function (x) { return x.r; });
  }

  /** 追加一条空白规则（priority 取现存最大值 + 10，避免与既有规则抢首命中）。 */
  function addRule(rules) {
    var arr = (rules || []).slice();
    var maxP = 0;
    arr.forEach(function (r) { var p = Number(r.priority) || 0; if (p > maxP) { maxP = p; } });
    arr.push({ path_regex: '', min_size: 0, max_size: 0, mode: 'single', priority: maxP + 10 });
    return arr;
  }

  /** 删除第 index 条（越界忽略）。 */
  function removeRule(rules, index) {
    var arr = (rules || []).slice();
    if (index >= 0 && index < arr.length) { arr.splice(index, 1); }
    return arr;
  }

  /**
   * 校验规则集。opts = { hardMax, volBlockMax }（任一缺省/0 则跳过该条比较）。
   * 返回 { ok: true, errors: [] } 或 { ok: false, errors: [{index, msg}] }。
   */
  function validateRules(rules, opts) {
    var o = opts || {};
    var hardMax = Number(o.hardMax) || HARD_MAX_SINGLE_BYTES;
    var volBlockMax = Number(o.volBlockMax) || 0;
    var errors = [];
    var arr = rules || [];
    if (arr.length > MAX_RULES) {
      errors.push({ index: -1, msg: '规则条数 ' + arr.length + ' 超过上限 ' + MAX_RULES });
      return { ok: false, errors: errors };
    }
    arr.forEach(function (r, i) {
      var re = String(r.path_regex || '');
      if (!re) { errors.push({ index: i, msg: 'path_regex 不能为空' }); }
      else if (re.length > 512) { errors.push({ index: i, msg: 'path_regex 长度超过 512' }); }
      else {
        try { new RegExp(re); } catch (e) { errors.push({ index: i, msg: 'path_regex 无法编译: ' + e.message }); }
      }
      var mode = String(r.mode || '');
      if (MODES.indexOf(mode) < 0) {
        errors.push({ index: i, msg: 'mode 非法（须为 single 或 random）' });
        return;
      }
      // 未提供（undefined/null/空串）视作 0 = 该侧不限；只有**写了但不合法**才报错。
      var min = isBlank(r.min_size) ? 0 : parseSize(r.min_size);
      var max = isBlank(r.max_size) ? 0 : parseSize(r.max_size);
      if (min === null) { errors.push({ index: i, msg: 'min_size 非法' }); }
      if (max === null) { errors.push({ index: i, msg: 'max_size 非法' }); }
      if (min !== null && max !== null && min > 0 && max > 0 && min > max) {
        errors.push({ index: i, msg: 'min_size 大于 max_size' });
      }
      if (mode !== 'single') { return; }
      if (!max) { errors.push({ index: i, msg: 'single 模式需给 max_size（否则整文件单块会让内存峰值不可控）' }); return; }
      if (max > hardMax) { errors.push({ index: i, msg: 'max_size 超过硬上限 ' + formatSize(hardMax) }); }
      if (volBlockMax > 0 && max > volBlockMax) {
        errors.push({ index: i, msg: 'max_size 超过卷块上限 ' + formatSize(volBlockMax) });
      }
    });
    return { ok: errors.length === 0, errors: errors };
  }

  /**
   * 前端行数组 → PUT body。
   *
   * 体积字段的取舍：用户**没动过**就原样提交原始字节值（精确）；动过则按输入解析并回写
   * 纯数字字节（不经过 formatSize 往返，避免 1234567 → "1.18MiB" = 1237319 的静默漂移）。
   */
  function toPayload(rules) {
    return {
      rules: (rules || []).map(function (r) {
        var out = {
          path_regex: String(r.path_regex || ''),
          mode: String(r.mode || 'single'),
          priority: Number(r.priority) || 0
        };
        var min = sizeField(r, 'min_size');
        if (min) { out.min_size = min; }
        var max = sizeField(r, 'max_size');
        if (max) { out.max_size = max; }
        if (r.note) { out.note = String(r.note); }
        return out;
      })
    };
  }

  /** sizeField 返回该行某体积字段应提交的值（未编辑 ⇒ 原始字节；已编辑 ⇒ 解析结果）。 */
  function sizeField(r, key) {
    var parsed = parseSize(r[key]);
    var orig = Number(r['orig_' + key]) || 0;
    if (orig > 0 && (parsed === null || parsed === orig)) { return orig; }
    return parsed || 0;
  }

  /** 后端 value（{rules,version}）→ 前端行数组。 */
  function fromValue(value) {
    var v = value || {};
    var rules = Array.isArray(v.rules) ? v.rules : [];
    return rules.map(function (r) {
      return {
        path_regex: String(r.path_regex || ''),
        min_size: Number(r.min_size) || 0,
        max_size: Number(r.max_size) || 0,
        mode: String(r.mode || 'random'),
        priority: Number(r.priority) || 0,
        note: r.note || '',
        // 原始字节值：用户**未编辑**该字段时原样提交，避免经过 formatSize（≤2 位小数）
        // 往返把 1234567 改写成 1.18MiB=1237319 这类静默精度损失（可能直接撞卷块上限）。
        orig_min_size: Number(r.min_size) || 0,
        orig_max_size: Number(r.max_size) || 0
      };
    });
  }

  /**
   * fromEffective 把分区回显（含 rules / baseline_rules / effective_source）解析为「要展示的行」：
   * **运行期规则为空时展示基线**（否则仅配 extra.block_rules 的卷在抽屉里是空表，运维看不到
   * 实际生效的规则）；并标记 fromBaseline 供保存时提示「将压制基线」。
   *
   * 这是本函数存在的唯一理由：只读 value.rules 会让基线配置在 UI 中「隐形」，用户随手新增
   * 一条规则保存后，运行期快照整体覆盖基线 —— 基线规则静默失效（审查 Important）。
   */
  function fromEffective(value) {
    var v = value || {};
    var runtimeRows = fromValue({ rules: v.rules || [] });
    var baselineRows = fromValue({ rules: v.baseline_rules || [] });
    var source = String(v.effective_source || '');
    if (runtimeRows.length > 0) {
      return { source: source || 'runtime', rows: runtimeRows, fromBaseline: false, baselineCount: baselineRows.length };
    }
    if (baselineRows.length > 0) {
      return { source: source || 'baseline', rows: baselineRows, fromBaseline: true, baselineCount: baselineRows.length };
    }
    return { source: source || 'volume_default', rows: [], fromBaseline: false, baselineCount: 0 };
  }

  /** saveWarning 返回保存前的警示文案（空前缀 = 无警示）。 */
  function saveWarning(eff) {
    if (!eff || !eff.fromBaseline || !(eff.baselineCount > 0)) { return ''; }
    return '当前展示的是**静态基线**（extra.block_rules，共 ' + eff.baselineCount +
      ' 条）。保存后将以运行期规则**覆盖**基线：基线中未在此列出的规则将不再生效（可从配置文件恢复）。';
  }

  var api = {
    HARD_MAX_SINGLE_BYTES: HARD_MAX_SINGLE_BYTES,
    MAX_RULES: MAX_RULES,
    MODES: MODES,
    parseSize: parseSize,
    formatSize: formatSize,
    sortRules: sortRules,
    addRule: addRule,
    removeRule: removeRule,
    validateRules: validateRules,
    toPayload: toPayload,
    fromValue: fromValue,
    fromEffective: fromEffective,
    saveWarning: saveWarning
  };

  if (typeof window !== 'undefined') { window.v2volumeSettings = api; }
  if (typeof module !== 'undefined' && module.exports) { module.exports = api; }
})();
