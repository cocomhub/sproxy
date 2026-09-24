# Grafana 面板

`sproxy-dashboard.json` 是 sproxy 官方 Grafana dashboard（Prometheus 数据源），覆盖：

- **请求**：速率 / 错误率（5xx 占比）/ 活跃连接
- **文件与云下载**：上传/下载吞吐与速率、云下载活跃数/成功率
- **卷 I/O**：速率与失败率
- **隧道与 hub**：mux 帧收发、hub 节点数/质量/重传率
- **传输层**：WS/QUIC/TCP 字节、mesh 拨号

## 导入

1. Grafana → Dashboards → Import → Upload JSON 文件；
2. 选择 Prometheus 数据源（变量 `DS_PROMETHEUS`）与 `job`（默认全部 `.*`）；
3. 刷新间隔默认 30s，时间窗口默认近 6h。

## 配套

- 指标端点在 `GET /metrics`（Prometheus 文本格式，40+ `sproxy_*` 指标）；
- 独立指标端口见配置 `metrics.port`（`metrics_auth` 令牌保护）。
