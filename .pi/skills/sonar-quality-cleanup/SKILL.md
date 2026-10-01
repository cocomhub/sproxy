---
name: sonar-quality-cleanup
description: >
  SonarQube 质量门禁纪律：开发/提交新功能（新命令、新结构体、新参数、新 JS、
  新依赖）前的防回归自检与强制校验、NOSONAR 行尾格式铁律、Sonar 专属规则与
  golangci-lint 盲区、本地 docker/sonar-scanner 前置门禁、go.work 覆盖聚合。
  在以下场景使用：提交新功能前自检；Sonar 复扫清零；覆盖率/Coverage 指标异常排查；
  CI 覆盖聚合与依赖重构；等价重构（S107/S3776/JS 现代化）后质量验证。
---

# Sonar 质量门禁纪律（契约）

> 战役成果：sproxy Sonar 存量 1252 → 2（98.5%），见 `docs/archive/sonar-ci-experience.md` 完整踩坑。

## 〇、提交前强制门禁（本地跑通，禁止新 issue）

- **为什么必修**：CI 的 sonar job **不阻断合并**（`sonarqube-scan-action` 无 `qualitygate.wait`），
  且 golangci-lint 覆盖不到 Sonar 专属规则（见下）——**门禁只在本地自觉**。
- **开新功能前，若本机有 docker/可装 sonar-scanner，先本地跑 Sonar 确认无新增问题**：
  ```bash
  # 有 docker（无需全局装 scanner）
  docker run --rm -v "D:/workdir/leon/cocomhub/sproxy:/usr/src" \
    -e SONAR_TOKEN=<token> -e SONAR_HOST_URL=https://sonarcloud.io \
    sonarsource/sonar-scanner-cli
  # 或本机装了 sonar-scanner
  make sonar-analyze
  ```
  重点盯 **New Code（新代码）** 的 `issues` 数 = 0，不是看 Overall 存量。
- **红线**：`make lint` / golangci 全绿 ≠ Sonar 绿（S8242/S3776/JS 规则 lint 测不到）。

## 一、新功能防回归自检清单（提交前逐条过）

每条都曾真实发生并修复（括号为 PR），是新功能最容易重复踩的坑：

| # | 自检项 | 触发即做 | 证据 |
|---|--------|---------|------|
| 1 | 新函数参数 >4 | 立即分组为选项/上下文结构体，不等 Sonar 报 S107 | #692/694/695/696（56 个收编） |
| 2 | 新结构体含 `context.Context` | 单操作作用域+行尾 NOSONAR，或重构不放 ctx | #701 S8242 |
| 3 | 新 JS promise 串 | async/await 扁平化（S9383/S9381/S9382） | #697/#708 |
| 4 | 高认知复杂度函数 | 拆 helper / 早返回，≤15 | P3 ×20 PR |
| 5 | 新文件/新模块依赖 | go.sum 存在（**空 go.sum 也算**）、module 加入 `test-cover-all` | #711/#712 |
| 6 | 写 NOSONAR | 必须行尾格式（见下），先确认规则是 lint 盲区 | #692/698→704 |
| 7 | 新增并行测试 | 共享注册表用唯一键不 Clear()；MinIO 容忍 TOCTOU；超时按 stall 校准 | #701/709/601 |

## 二、NOSONAR 格式铁律（SonarGo 只识别行尾）

- ❌ 错误：`// NOSONAR` 独立上一行（理由在上）——SonarGo 忽略，白加。
- ✅ 正确：`code // NOSONAR: 规则 — 理由` 在被标记行**行尾**。JS 同理（S9382 也需行尾）。
- 铁证：#692/698 独立上一行 → 复扫仍 OPEN；#704 全改行尾 → 清零。

## 三、Sonar 专属规则 vs lint 盲区

| 规则 | 触发场景 | lint 能查 |
|------|---------|----------|
| S8242 | 结构体持有 `context.Context` 字段 | ❌ |
| S8196/S8209/S8205/S8184 | 命名/参数/嵌套结构 | ❌ |
| S3776/JS（S9383/S9381/S9382） | 复杂度/JS 现代化 | ❌ |

**约束**：等价重构（S107/S3776/JS）后必须本地 Sonar 复扫，不能只信 lint + 审查 agent。

## 四、go.work 覆盖聚合（#712）

- `go test ./...` 在 go.work 模式只测当前 module → Sonar 把 16 子 module 按 0% 稀释。
- 聚合：子 module **cd 进入 + GOWORK=off + coverprofile 绝对路径**（Windows 用 `D:/`）→ `build/coverage/<slug>.out`，`reportPaths=build/coverage/*.out,build/cover.out`。
- `SKIP_ROOT_COVER=true`：test job 产 root.out，test-submodules 跳根。

## 五、Coverage 口径（避免误判）

- 本地 cover-check = 根 module = 语句覆盖 81.4%；Sonar 总体 = 全仓行覆盖（根+子 module、分母含 test）= 33.3%。
- 差 2.4 倍正常（语句跨 1~3 行 + 范围不同）。**Sonar 覆盖率数字参考价值有限，价值在 issue 规则维度**。
- 提升 = 补低覆盖子 module 测试（baidupcs/s3/sclient），不是刷分。

## 六、新 issue 门禁（CI 已接 quality-gate-action）

CI 的 sonar job 扫码后用 `sonarqube-quality-gate-action` 等待 **New Code** gate 结果，
FAILED 即 job 红 → 可作为 required check 阻断合并：

```yaml
- name: SonarQube Scan
  uses: sonarsource/sonarqube-scan-action@... # 只扫描，不等待 gate
  env: { SONAR_TOKEN: ${{ secrets.SONAR_TOKEN }} }
- name: SonarQube Quality Gate check
  id: gate
  uses: sonarsource/sonarqube-quality-gate-action@... # v1.2.1（pin SHA）
  with: { pollingTimeoutSec: 600 }
  env: { SONAR_TOKEN: ${{ secrets.SONAR_TOKEN }}, SONAR_HOST_URL: ${{ secrets.SONAR_HOST_URL }} }
```
注意：`sonarqube-scan-action` 本身**没有 `qualitygate.wait` 输入**，必须靠独立
quality-gate-action 回流 `report-task.txt`。**平台侧**（SonarCloud 项目 Quality Gate 对
「新代码」设条件）需在 sonarcloud.io 配置，CI 侧只负责回流与红 job。