# Web UI 交互缺陷审计（原型 v3）

- 日期：2026-11-24
- 对象：`docs/designs/mockups/webui-v2-prototype.html`
- 触发：用户反馈「控制台点一下，概览/云端任务全部选中」「为什么有两份」

## 1. 根因（全部已修复）

| # | 缺陷 | 证据 | 根因 | 修复 |
| --- | --- | --- | --- | --- |
| R1 | 打开控制台后，侧栏 6 个入口（概览/云端任务/待处理/审计/卷/配置）全部高亮 | 打开控制台后可见 `aria-current` 元素 **6 个** | 侧栏这 6 项都写成 `data-goto="console"`，`show()` 用「goto 相等」判定当前项 ⇒ 全部命中 | 侧栏收敛为**单个「控制台」入口**；`setNav` 增加 `!hasAttribute('data-sec')` 防御 |
| R2 | 桌面出现第二份导航（页面底部多出一条「文件/传输/回收站/控制台」） | 1500px 下可见导航条目 **8 个**（应为 4） | `.mobile-nav` 只在 `@media (max-width:640px)` 里写了 `display:flex`，**缺少桌面默认 `display:none`** | 补 `.mobile-nav{display:none}` 基线规则 |
| R3 | 手机端顶部 tab 与底部导航同时出现，出现两个「文件/传输/回收站」选中 | 390px 下 C 方案 `aria-current` = 2 | `[data-skin="c"] .top-tabs{display:flex}` 特异性高于媒体查询里的 `.top-tabs{display:none}` | 移动端加同特异性 `[data-skin="c"] .top-tabs{display:none}` |
| R4 | 控制台外层入口与内部分区**同时**高亮（父 + 子） | 打开控制台后 `aria-current` = 2（控制台 + 概览） | 语义粒度混用：外层是容器，内部才是当前分区 | 外层改 `data-open="1"` 弱提示（仅底色），`aria-current` 只留给内部 `.cnav` |
| R5 | 可点击区小于 24×24（WCAG 2.5.8 AA） | A 行内按钮 **26×22**；C 行内「⋯」**23×21**；移动端设计稿工具条被 `scale(.92)` 压到 **23.9px** | 图标按钮高度按视觉定，未按最小命中区校验；移动端对工具条整体缩放 | A 行内 26×24；C 最小 24×24；移动端取消缩放改为换行布局 |

## 2. 修复后的定量验证

| 检查 | 结果 |
| --- | --- |
| 三套皮肤下打开控制台 → 点「云端任务」 | 可见 `aria-current` = **1**（云端任务），`data-open` = **1**（侧栏控制台） |
| 桌面导航条目数（1500px） | 4（文件/传输/回收站/控制台），无第二份 |
| 手机导航（390px） | 顶部 tab 隐藏，仅底部导航 4 项 |
| 全状态审计（57 状态 × 5 视口 1500/1280/1024/768/390） | **0 问题** |
| 重复 id | 0 |

## 3. 审计方法（可重复）

自动检查项（headless Chrome 注入脚本，逐状态执行）：

| 检查 | 判据 |
| --- | --- |
| 导航多重选中 | 可见 `[aria-current]`（导航/分区）数量 ≤ 1 |
| 横向溢出 | `documentElement.scrollWidth - clientWidth ≤ 2` |
| 控件文字裁切 | `.btn/.chip/.cnav/.nav/顶栏 tab/底部导航` 的 `scrollWidth ≤ clientWidth + 1` |
| 命中区 | 可见按钮 `width ≥ 24 && height ≥ 24` |
| 顶栏被遮挡 | 按钮中心 `elementFromPoint` 必须是自身或后代（弹窗打开时跳过） |
| 重复 id | 全文档 `id` 唯一 |

状态矩阵（57 个）：每套皮肤 × {文件列表, 网格, 传输, 回收站, 控制台 12 分区, 控制台空/错/加载三态}
= 3 × 19。

## 4. 截图（人工复核用）

按状态批量生成 **31 张**（脚本：`docs/designs/mockups/shoot_states.py`）：

```
%TEMP%\webui-shots\
  a-files.png  a-grid.png  a-transfer.png  a-trash.png
  a-console-overview.png  a-console-cloud.png
  a-console-empty.png  a-console-error.png  a-console-loading.png
  b-*.png  c-*.png  a-dark-files.png  c-dark-console.png
  m-a-files.png  m-c-console.png
```

> 说明：截图用于人工视觉复核；**本报告的“缺点”结论来自 DOM 度量与对比度计算**，不是对图片的主观描述。

## 5. 其他同时修掉的问题

| 问题 | 说明 |
| --- | --- |
| `text-3` 表头对比度不足 | 表头改用 `--text-2`（a/light 4.35 → 通过） |
| 状态 pill 文字对比度不足 | 半透明底叠色按 12% 计算后上调状态色（9 处） |
| 表格数字跳动 | 统一 `tabular-nums` |
| 焦点丢失 | 浮层关闭后焦点回到触发元素；浮层内 Tab 循环 |

## 6. 仍存在的限制

1. 没有人工视觉评审（模型无法读图），截图需人来判断「层级/美观/信息密度」；
2. 审计只覆盖结构性问题（选中态、溢出、裁切、命中区、遮挡、对比度），不覆盖「文案是否清楚」「操作是否顺手」；
3. 数据为假数据，真实长文件名/大数值/极端状态（0 项、1000+ 行）尚未压测；
4. 原型不是产品代码，未跑真实浏览器 e2e（落地后在 `web/e2e` 补齐）。
