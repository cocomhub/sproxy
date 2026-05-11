# Tasks
- [x] 任务 1：创建命令目录骨架
  - [x] 新增目录 cmd/sproxy 与 cmd/sclient（实现阶段放置 main 包）
  - [x] 预留包结构与命名规范（目录名即二进制名）
- [x] 任务 2：编写 Makefile 基础规则
  - [x] 定义 BIN_DIR=build/bin 并确保目录存在
  - [x] 自动发现 CMD_LIST（使用 go list ./cmd/* + notdir）
  - [x] 统一目标 build：遍历 CMD_LIST 输出 build/bin/<name>[.exe]
  - [x] 增加单目标 build-%（如 build-sproxy）
  - [x] 增加 clean：清理 build/bin
  - [x] 可通过环境变量 GOOS/GOARCH 覆盖，默认使用本机
- [ ] 任务 3：验证本地构建
  - [x] 在当前环境执行构建，生成 sproxy(.exe)、sclient(.exe)
  - [x] 验证 build 产物位置与清理策略
- [ ] 任务 4：文档更新
  - [x] 在 README 增加构建与产物目录说明

# Task Dependencies
- [任务 2] 依赖 [任务 1]
- [任务 3] 依赖 [任务 2]
- [任务 4] 依赖 [任务 2]
