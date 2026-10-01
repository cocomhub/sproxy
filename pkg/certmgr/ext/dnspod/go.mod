// 本模块无外部依赖图（唯一依赖为 replace 到本地路径的根模块），无可锁条目；
// 提供空 go.sum 锁文件（S8566 锁文件存在性检查；本地 replace 不经 go.sum 校验）。
module github.com/cocomhub/sproxy/pkg/certmgr/ext/dnspod

go 1.27

replace github.com/cocomhub/sproxy => ../../../..

require github.com/cocomhub/sproxy v0.0.0-00010101000000-000000000000
