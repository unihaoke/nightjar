# Grafana 大盘目录（放进来 30 秒内自动生效）

平台已自动完成两件事：

1. **数据源**：`deploy/grafana/provisioning/datasources/prometheus.yml` 把 `mwops-prometheus`
   设为默认数据源（指向平台自带的 Prometheus）——不需要手工添加；
2. **加载器**：`deploy/grafana/provisioning/dashboards/default.yml` 会导入本目录下的 JSON。

## 怎么加一个大盘

1. 打开 <https://grafana.com/grafana/dashboards/>，按编号找到官方大盘（本平台集成中心
   每张组件卡片上已经给出编号）：

   | 组件 | 大盘编号 |
   |---|---|
   | Redis | 763 |
   | MySQL | 7362 |
   | PostgreSQL | 9628 |
   | Kafka | 7589 |
   | Elasticsearch | 2322 |
   | Nginx | 9614 |

2. 该页面右侧 **Download JSON** → 把文件放到本目录（文件名随意，如 `redis-763.json`）；
3. 等 30 秒或重启 Grafana 容器即可在 Grafana 的 Dashboards 里看到。

> 大盘里的数据源变量选 `mwops-prometheus`；实例筛选变量选 `instance_name`
> （平台写入的标签，值就是「集成名称」，如 `legacy-redis`）。

## 为什么不预置一堆 JSON

官方大盘动辄上千行、还会随版本变化，直接内置容易与当前 Exporters 的指标名漂移。
推荐做法是：**按需要导入官方大盘**（编号由集成中心给出），自建大盘则导出 JSON 后放进来。
