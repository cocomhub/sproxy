# Tasks

- [x] Task 1: 实现配置文件管理（config.go）
  - [x] 定义 SclientConfig 结构体，包含 server_url、upload_endpoint、download_endpoint、delete_endpoint、check_md5、timeout 字段
  - [x] 实现 LoadConfig 函数：从 ~/.sclient.yaml 加载配置，文件不存在时创建默认配置
  - [x] 实现 SaveConfig 函数：将配置写入 ~/.sclient.yaml
  - [x] 实现 config show 子命令：打印所有配置项
  - [x] 实现 config set 子命令：更新指定配置项并保存

- [x] Task 2: 实现 HTTP 客户端操作（client.go）
  - [x] 实现 calculateMD5 函数：计算文件 MD5 值
  - [x] 实现 uploadFile 函数：POST multipart/form-data 上传单个文件，携带 X-File-MD5 头，解析 JSON 响应
  - [x] 实现 downloadFile 函数：GET 下载文件到本地，支持进度显示，可选 MD5 校验
  - [x] 实现 deleteFile 函数：POST 删除文件，携带 X-File-MD5 头，解析 JSON 响应
  - [x] 实现 listFiles 函数：GET /api/files 获取文件列表并显示

- [x] Task 3: 实现进度条显示（client.go）
  - [x] 实现进度条 writer，包装 io.Reader/io.Writer，在传输过程中实时打印进度

- [x] Task 4: 实现 CLI 路由与帮助信息（main.go）
  - [x] 实现子命令路由：upload/download/delete/list/config/version/help
  - [x] 实现全局选项解析：-s/--server、--no-md5、-o/--output、-v/--verbose
  - [x] 实现帮助信息：显示所有子命令和选项说明
  - [x] 实现 version 子命令：显示版本号、构建时间、配置摘要
  - [x] 无参数时默认显示帮助

- [x] Task 5: 验证构建
  - [x] 运行 `go build ./cmd/sclient/...` 确保编译通过
  - [x] 运行 `go vet ./cmd/sclient/...` 确保无 vet 警告

# Task Dependencies
- Task 2 依赖 Task 1（HTTP 操作需要配置结构体）
- Task 3 依赖 Task 2（进度条嵌入上传/下载流程）
- Task 4 依赖 Task 1、Task 2（CLI 路由调用配置和客户端操作）
- Task 5 依赖 Task 1-4