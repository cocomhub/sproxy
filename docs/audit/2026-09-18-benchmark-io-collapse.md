# Benchmark CI I/O 塌陷第三形态处理方案（2026-09-18）

## 问题
- 已知：单 op 停滞守卫（>2s fail，benchwatch）+ 失败 rerun 处置
- 第三形态：单 op 不超阈值但**聚合慢**——runner I/O 塌陷时 N 自适应超调 → 整体 6 分钟 CANCELLED（timeout-minutes 兜底触发）

## 推荐方案（先取证，勿动 timeout-minutes）
在 `make bench` 侧加**总时长/渐进速率守卫**（非 CI timeout——CI timeout 是兜底非检测）：

### 1. 总时长守卫（简单，先做）
- bench 总执行超 **5 分钟** → 提前终止 + 打印「疑似 I/O 塌陷，请 `gh run rerun <id> --failed`」
- 位置：benchwatch 或 make bench 包装

### 2. 渐进速率守卫（增强，可选）
- 每 N 秒检查已完成 op 数 vs 预期（塌陷时速率骤降）——比总时长更早发现
- 阈值：连续 2 个检查窗口速率 < 预期 30% → 判塌陷

### 3. 不动 CI timeout-minutes
- 保留为最终兜底；检测靠 bench 侧守卫（fail 更快 + 提示重跑）

## 实施
- benchwatch 加总时长参数（--max-duration 5m）+ 塌陷提示
- 或 make bench 加 timeout 包装（timeout 5m benchwatch ... || echo 提示）
