// 本模块无外部依赖图（唯一依赖为 replace 到本地路径的根模块），无可锁条目故无 go.sum；
// 依赖校验由根模块 go.sum 承担（go mod tidy 后仍不生成）。
// NOSONAR
module github.com/cocomhub/sproxy/pkg/certmgr/ext/dnspod

go 1.27

replace github.com/cocomhub/sproxy => ../../../..

require github.com/cocomhub/sproxy v0.0.0-00010101000000-000000000000
