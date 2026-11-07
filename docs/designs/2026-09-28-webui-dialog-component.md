<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 统一对话框/确认组件（roadmap 13.1 / V4）设计

> 日期：2026-09-28 ｜ 状态：头脑风暴（待评审） ｜ 范围：替换原生 `confirm()`/`prompt()`/`alert()` 为统一可定制对话框组件
> 关联：[2026-09-28-webui-wysiwyg-feedback.md](./2026-09-28-webui-wysiwyg-feedback.md)（V6，反馈体系）、[2026-09-28-webui-responsive-layout.md](./2026-09-28-webui-responsive-layout.md)（V2，布局）。

## 1. 背景与目标

### 1.1 现状（源码实证，app.js 多处）

- 删除/清空等危险操作全部用**原生 `confirm('确认删除...?')`**（app.js:338 等）——浏览自带弹窗：
  - 样式不可控（与主题割裂）；
  - 无危险分级（删除/清空与普通操作同一样式）；
  - 无附加信息/默认焦点/键盘 Esc 统一行为；
  - 无 Promise 化（阻塞式，流程难组合）；
  - 移动端体验差。

### 1.2 目标

- `showConfirm({title, desc, danger, okText, cancelText, onOk, onCancel})`——**统一确认组件**（Promise 化可选）；
- `showPrompt({title, label, placeholder, value, onOk})`——统一输入对话框（替换原生 prompt）；
- 危险分级：danger 按钮红色渐变 + 震动/聚焦确认；
- 键盘：Esc 取消、Enter 确认、Tab 循环焦点；ARIA（role=dialog / aria-modal / focus trap）；
- 主题联动（暗色变量）。

### 1.3 非目标

- 不做 toast 系统（已有 showToast）；不做多模态复杂表单。

## 2. 组件与接口

### 2.1 纯逻辑层（node:test 直测，新 `dialog.js`）

```js
// 生成对话框骨架 HTML（纯字符串，无 DOM）
buildConfirmHtml({title, desc, danger, okText, cancelText}) -> string
buildPromptHtml({title, label, placeholder, value}) -> string
// 键盘映射：Esc→cancel, Enter→confirm（纯函数返回动作）
dialogKeyAction(key, which) -> 'confirm'|'cancel'|null
```

### 2.2 渲染层（app.js）

```
showConfirm(opts):
  mount buildConfirmHtml → 焦点陷阱（tab 循环）
  绑定 Esc/Enter/点击遮罩（cancel）
  返回 Promise<boolean>（onOk 触发 resolve）
替换全仓原生 confirm/prompt 调用（删除/清空/批量/目录删除等）
```

## 3. 错误处理

- 无 onOk/onCancel 回调 → resolve(false) 安全；
- 连续多次 showConfirm → 队列化（互斥）防重叠；
- 遮罩点击 vs Esc → 均 cancel（danger 操作不误确认）。

## 4. 测试 + 变异点

| 用例 | 断言 | 变异 → 应红 |
|------|------|------------|
| buildConfirmHtml | danger 类名/按钮文本/desc 转义正确 | 不转义 → XSS 测试红 |
| dialogKeyAction | Esc→cancel、Enter→confirm | 映射错 → 红 |
| focus trap（Playwright） | Tab 循环不出对话框 | 无 trap → e2e 红 |
| 队列（Playwright） | 连续确认不重叠 | 无队列 → e2e 红 |
| 全仓替换 | 源码无原生 confirm(/prompt( 残留 | 残留 → 静态检查红 |

## 5. 片划分

- **P1**：dialog.js 纯函数 + showConfirm/showPrompt + 全仓替换 + node test
- **P2**：focus trap / 队列 / ARIA 打磨 + Playwright e2e

## 6. 风险与零回归

- 全仓替换后行为语义不变（确认/取消等价）；danger 样式默认关闭（默认确认按钮 secondary）；
- 新组件默认暗色/亮色双主题联动；与 V2/V3/V6 独立可并行。