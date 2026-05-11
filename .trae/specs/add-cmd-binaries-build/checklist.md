* [x] 存在目录 cmd/sproxy 与 cmd/sclient，且为 main 包

* [x] 运行 make build 后生成 build/bin/sproxy\[.exe] 与 build/bin/sclient\[.exe]

* [x] 运行 make build-sproxy 仅生成 sproxy 可执行文件

* [x] 运行 make clean 后 build/bin 目录被清理或为空

* [x] Makefile 能自动发现新建的 cmd 子目录并正确输出二进制

* [x] 允许通过 GOOS/GOARCH 覆盖目标平台（默认本机生效）
