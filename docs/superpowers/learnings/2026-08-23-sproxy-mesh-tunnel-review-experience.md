# sproxy feature/mesh-tunnel 分支代码审查与修复经验

> 日期：2026-08-23
> 范围：Hub 中继 / WebRTC 打洞 / P2P 直连 / mesh 内网穿透（58 文件，+6952/-577）
> 结果：21 独立审查代理 → 206 项问题（4 critical / 78 important / 123 suggestion）+ H1 搜索新增 3 important → 17 修复批次 + 残留收尾，全部复检通过，Phase 5 最终审查 🟢 可合并

## 通用问题模式（本次反复出现的类型）

1. **泵送/关闭挂起模式**（5+ 处重复）：双向 `io.Copy` + `wg.Wait()` 等双方向，远端先断而本地未 close 时永久挂死。正确范本：`done` channel + 双侧 `Close()`（socket 适用）或**方向区分通道**（`select { case <-outDone: return; case <-inDone: <-outDone }`，stdin 侧 `Close()` 无法解除阻塞读，需等对端写完）。同款 bug 会跨文件重复出现——修复一处后必须 grep 同类模式。
2. **认证域错配**：relay_token（WS 注册认证）与 auth_token（HTTP Bearer）是**两套独立认证域**。跨域使用（信令走 authMiddleware 却传 relay_token）导致静默失败。
3. **生产死代码**：选路抽象（p2p.Plan + hub.Path）、handler 工厂（buildRelayHandler）、信令类型（SignalCandidate）多处"有声明无执行"——被内联实现替代后未清理（`f56baed` 接线时遗漏）。死代码+TODO 比删除更糟（会被误认为待办工作）。
4. **mapstructure 标签与 yaml 键不一致**：`http_01` vs `http01`，viper 解码路径恒为默认值（本分支引入的回归）。防同类漂移：反射遍历配置树断言 `yaml 标签 == mapstructure 标签`。
5. **并行修复的 git 冲突**：多个修复代理共享工作区时，`git commit --amend` 会卷入其他批次变更、`git reset` 回退对方变更。**串行修复优先**；必须并行时禁止 `git reset/checkout/stash`，精确 `git add`。

## 流程改进（本次验证有效的做法）

1. **跨代理交叉验证（⨯N）优先级上调**：信令 X-Node-ID 身份可伪造被 3 个独立代理发现（hub-signaling F9 / hub-core P13 / server-signaling #1），三重印证后定级为最高优先级安全项。汇总阶段必须标记"被多个独立代理报告"的问题。
2. **H1 同类问题搜索的价值**：按已发现类型全仓库 grep，发现 30-60% 额外问题（service_registry.go 整文件第二套选路抽象、WebRTC ICE candidate 注入、relay_dial 每连接挂起）。
3. **4D 复检发现跨批次接线缺口**：B3 复检发现"mesh/p2p 客户端未携带 per-node secret"（服务端强制校验后信令 403）——这是 B2 只给 relay.go 接入的遗漏。复检不只是"确认修好"，更要查**调用链上未同步的接线点**。
4. **文档批次最后做**：文档描述依赖代码落地后的准确 flag/行为（C4 云端推数据命令需 B9/B13/B17 修完后才能写对）。文档与代码一致性用 grep 核对命令 flag。
5. **安全审查插件作为补充反馈**：后台安全审查（security-guidance）发现的 MEDIUM 项需逐一核实触发条件（如 `InsecureSkipVerify` 仅 `--insecure` 置位时生效 = 用户已批准的有意权衡），合理则记录不改变已批准方案。

## 测试最佳实践（本次沉淀）

1. **SetHostOnly 消除真实 STUN 依赖**：webrtc 测试从 ~124s 降至 3.3s。网络测试用 host-only 进程内回环替代真实 STUN，CI 稳定且覆盖 DataChannel 全流程。
2. **死测试识别**：`if err != nil { t.Log(); return }` 无断言、`_ = err` + `default` 静默通过、名称声称行为但测试走其他分支——都制造"已覆盖"假象。修复前先跑一次确认测试实际断言什么。
3. **E2E 固定 Sleep 改轮询**：`time.Sleep(1s)` 等待注册在 -race/CI 下 flaky。改轮询 `/api/hub/nodes` + 数据面"全 echo 往返"验证（dial→写→带 deadline 读→匹配）。
4. **测试注入点**：不可注入的常量（PollTimeout 25s）导致空 poll 阻塞 25s——测试通过注入字段/构造参数使超时可配。

## API 设计经验

1. **per-node secret 可扩展能力字段**：注册帧用 `Capabilities []string`（slice 而非单一 bool），未来可扩展其他能力标志。本分支线上未使用故不做向后兼容双轨，直接统一格式。
2. **Go 变参保持向后兼容**：`NewRegisterFrame(nodeID, token, meta, caps ...string)` / `NewHubSignaler(..., secret ...string)`——变参让现有调用零改动，测试零破坏。
3. **ServeOptions 门控协议**：`DialResultFrames` 布尔门控区分"经 hub 中继"（relay start=true，需结果帧感知拨号失败）vs "webrtc 直连"（p2p listen=false，结果帧会污染数据流）。同一叶子代码服务两种数据面。
4. **协议先 Encode 后消费**：poll 的 `Peek`+`Confirm` 原语——Encode 到 buffer 成功后才消费消息，避免 Encode 失败丢消息（HTTP 无确认协议下的最小可靠）。

## 修复统计

- 4 critical 全部修复（leaf pump 泄漏/截断、/ws 绕过鉴权、webrtc 死测试、文档不可复现）
- 78 important：74 完整修复 + 4 残留（Purge 钩子补挂、文档残留、端口截断升级等——残留收尾批次全部处理）
- 123 suggestion：随批次处理大部分，未处理项记录在 Phase 5 报告
- 无回归；所有破坏性变更（relay_token 强制、/ws 固定、per-node secret、-l loopback 归一、dial 结果帧协议）均在 commit message 显式标注
