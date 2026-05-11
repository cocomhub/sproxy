# Checklist

- [x] downserver/main.go 所有端点（/upload, /download, /delete, /{host}/{filepath...}, /bandwidth）在 sproxy 中均有对应实现
- [x] downserver 的 MD5 校验逻辑在 sproxy 中保持一致
- [x] downserver 的带宽统计逻辑在 sproxy 中保持一致
- [x] downserver 的优雅关闭逻辑在 sproxy 中保持一致
- [x] sproxy 的 transfer 端点增加了 host 白名单和 hop-by-hop header 剥离增强
- [x] sproxy 额外提供了 /healthz 和 /version 端点
- [x] fileclient.sh 的 upload/download/delete/list/config/version/help 功能在 sclient 中均未实现
- [x] sclient/main.go 当前为空壳（仅含空 main 函数）
- [x] 分析结论已记录在 spec.md 中