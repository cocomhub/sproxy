# CI 耗时优化全集（2026-10-27 五批收尾）

> 背景知识归档：2026-10-27 对 sproxy CI 的完整耗时优化（5 个 PR：#862/#863/#865/#867/#868/#869），
> 含每批的背景、改动、实测收益、踩坑与「已验证不做的候选」。供后续维护者直接沿用方法论，
> 不必重复评估已否定的方案。
>
> 前置阅读：`docs/archive/ci-throughput-wait-injection.md`（第一批的详细根因与等待注入方案）。

## 效果总览

| 指标 | 首日基线 | 五批后 | 变化 |
|---|---|---|---|
| 关键路径（最慢 job） | 926s | **449s** | **-51%** |
| Test (ubuntu, +Vault) | 891s | 276s→389s* | -56% |
| Test (windows) | 602s | 449s | -25% |
| E2E (windows) | 611s | 357s | -42% |
| Chaos | 312s | 265s | -15% |
| Test Sub-Modules | 275s | 260s | -5% |
| Lint | 224s | 203s | -9% |
| UI E2E | 208s | 228s | ±0（Playwright 缓存尚未命中首次） |
| SonarQube | 204s | 177s | -13% |

\* Test(ubuntu) 波动 276-389s（runner 负载）；最慢 job 已变为 Test (windows) 449s。

## 五批改动清单

| PR | 内容 | 落点 | 关键收益 |
|---|---|---|---|
| #862 | 等待上限测试注入 + 覆盖率单遍（初版） | pkg/files、pkg/server、Makefile、ci.yml | pkg/server -race 234-300s→66-75s |
| #863 | pre-commit SPDX 增量化为 addlicense -check | Makefile、.githooks/pre-commit | 每次 commit 全仓 addlicense 4-8s→<0.5s |
| #865 | e2e 二进制统一经 e2eBinPath（整包只 build 一次） | test/ 5 文件 | 6 处内联 go build → 1 次 |
| #867 | 覆盖并入第一遍测试 + fuzz 减半 + gofix-all 并行 | Makefile、ci.yml | Test job -271s；Chaos -37s；Lint -20s |
| #868 | Playwright 浏览器二进制缓存 | ci.yml | UI E2E 冷装 chromium 25s→缓存命中 2-3s |
| #869 | Test job 剔除 archcheck 的 race 全量 | Makefile、ci.yml | -87s windows / -70s ubuntu |

## 关键认知（踩坑沉淀，后续直接复用）

1. **go 测试缓存命中条件**：`-coverprofile` / `-race` / `-count` 改变命令行 ⇒ **缓存 key 不同 ⇒ 第二遍必重跑**。
   - #862 的「make test + cover-test 复用缓存」在 CI 实测**失效**（go test 295s + cover-test 271s 都重跑）。
   - 正解：#867 把 `-coverprofile` **并入第一遍**（`make test TEST_COVER_PROFILE=<path>`），删第二遍。
2. **waitq 假时钟不能加速 timer 超时**：`waitAcquire` 用 `time.NewTimer(timeout)`（真实墙钟）；Clock 接口只服务 `Now/Since`（老化/统计）。「配额打满断言 507」用例只能**注入短超时**（50ms），不能推假时钟。
3. **archcheck 的 race 是纯耗时**：88 个测试全做**全仓扫描**（spawn go list/go build 子进程），无并发功能代码 ⇒ race 检测器零增益，却把 13.5s 放大到 63-83s。门禁只需 Lint job 的非 race 版。
4. **`go list ./...` 输出完整 import path**：`TEST_EXCLUDE` 用短名（`internal/archcheck`）grep 不匹配 ⇒ 必须 `grep -vE '^github[.]com/cocomhub/sproxy/<包>$'`。
5. **addlicense 的 hasLicense 判定**（源码 main.go:432）= `bytes.Contains(lower(b[:1000]), "spdx-license-identifier")` ⇒ 任意 SPDX 值（写错的）都视为已有头；`-check` 只抓「完全无头」。json 不在 fileExtension 白名单（main.go:294-377）⇒ 不注入不检查。
6. **SPDX 不只对 go 文件**：仓库 116 个非 go 文本源码（web/static/*.js、scripts/*.sh、config.example.yaml 等）同样要求头（make addlicense 全量注入覆盖）。

## 已验证「不做」的候选（避免重复评估）

| 候选 | 结论 | 依据 |
|---|---|---|
| Sonar needs 收窄为 [test] | **不做** | 实测 test 276s > submodules 244s ⇒ Sonar 开始恒由 test 决定，无收益；且丢子 module 覆盖率 |
| Build matrix 合并为一 job | **不做** | 6 job 并行 → 串行 1 job 让墙钟变差；Build 144s 远非关键路径 |
| waitq 加可注入 timer 工厂 | 未来可选 | 需改 waitq 内部；当前无场景必须 |

## 残余（继续优化方向，按性价比排序）

1. **Test (windows) 449s**：`pkg/volume/secretdata` 130s + `pkg/server` 112s（Windows IO + race）。需 profile 定位，中等复杂度。
2. **Test (ubuntu) 波动**：389s 时含 Vault L2/L3（真实服务）；可评估 vault 用例降并行。
3. **pkg/cloud Cancel/Integrity 组 flake**（既有时序问题，master 基线 3/11 复现）：`RetryDelay: 1ms` + WaitFor 5s 在 -race 全量并发下偶发超时——独立登记待办，勿与其它优化混修。

## 方法论

- 每批改动前先用 GitHub Actions API 取**逐 job / 逐 step 实测**，再本地复现（-json 输出 per-test 时长）。
- 判断收益看**关键路径**（最慢 job），不看平均。
- 改 Makefile/ci.yml 后：`make -n` 走查 + 小范围实跑验证；hook 类改动按 `.githooks/HOOKS_VERIFICATION.md` 场景清单回归。
- 所有改动在独立 worktree 进行（主仓只读纪律），PR 后清分支 + worktree。
