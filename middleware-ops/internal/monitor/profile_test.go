package monitor

import "testing"

// 本文件锁定指标画像中「阈值方向」的正确性。
//
// 背景：evaluateStatus 对 HigherIsWorse=false 的指标采用「越低越差」判定，
// 即 value <= WarningThreshold 记 warning、value <= CriticalThreshold 记 critical。
// 曾出现过把「越低越差」指标按「越高越差」写阈值的错误：
//   - es.cluster_status 写成 Warning=1/Critical=0，导致集群 green（值 2）被判为 warning；
//   - es.node_count 写成 Warning=3/Critical=1，导致 5 节点健康集群被判为 warning。
// 这类错误不会导致构建失败，只会让监控面板与规则结果失真，因此用测试固化。

func TestProfileThresholdsAreSane(t *testing.T) {
	for _, mwType := range SupportedTypes() {
		profile := ProfileOf(mwType)
		if len(profile.Metrics) == 0 {
			t.Fatalf("中间件类型 %s 没有任何指标画像", mwType)
		}
		for _, spec := range profile.Metrics {
			spec := spec
			t.Run(mwType+"/"+spec.Name, func(t *testing.T) {
				if spec.Expr == "" {
					t.Fatalf("指标 %s 缺少 PromQL 表达式（无法查询）", spec.Name)
				}
				if spec.DisplayName == "" {
					t.Fatalf("指标 %s 缺少展示名", spec.Name)
				}

				warn, crit := spec.WarningThreshold, spec.CriticalThreshold
				switch spec.Mode {
				case ThresholdNone:
					return // 纯观测型指标：无阈值
				case ThresholdHigherWorse:
					// 越高越差：临界阈值必须不小于警戒阈值，否则 critical 永远不会先触发。
					if crit < warn {
						t.Fatalf("越高越差型指标的临界阈值(%v)小于警戒阈值(%v)", crit, warn)
					}
				case ThresholdLowerWorse:
					// 越低越差（含边界）：临界阈值必须严格小于警戒阈值，
					// 否则「严重」档不可达（相等）或方向写反（大于）。
					// 允许 Critical=0（ES red），因为判定用的是 value <= threshold。
					if crit >= warn {
						t.Fatalf("越低越差型指标的临界阈值(%v)必须严格小于警戒阈值(%v)，当前配置导致严重档不可达或方向相反", crit, warn)
					}
				case ThresholdBoolDown:
					// up 型：临界值应为 0（down），警戒档不参与判定。
					if crit != 0 {
						t.Fatalf("up 型指标的临界阈值应为 0，当前为 %v", crit)
					}
				default:
					t.Fatalf("指标 %s 的 ThresholdMode 非法: %q", spec.Name, spec.Mode)
				}
			})
		}
	}
}

// TestElasticsearchClusterStatusSemantics 显式锁定 ES 集群状态语义：
// 2=green 必须判 ok，1=yellow 必须判 warning，0=red 必须判 critical。
//
// 该用例覆盖了「0 作为有效临界值」这一易错点：判定必须用 <= 而非 <，
// 否则 value < 0 恒为假，red 会被误判为 warning。
func TestElasticsearchClusterStatusSemantics(t *testing.T) {
	spec, ok := SpecOf("es", "cluster_status")
	if !ok {
		t.Fatal("es.cluster_status 指标画像缺失")
	}
	cases := []struct {
		value float64
		want  string
		desc  string
	}{
		{2, "ok", "green"},
		{1, "warning", "yellow"},
		{0, "critical", "red"},
	}
	for _, tc := range cases {
		if got := evaluateStatus(spec, tc.value); got != tc.want {
			t.Fatalf("集群状态 %s（值 %v）应判定为 %s，实际 %s", tc.desc, tc.value, tc.want, got)
		}
	}
}

// TestElasticsearchNodeCountSemantics 锁定节点数语义：健康集群不得被误判为异常。
func TestElasticsearchNodeCountSemantics(t *testing.T) {
	spec, ok := SpecOf("es", "node_count")
	if !ok {
		t.Fatal("es.node_count 指标画像缺失")
	}
	if got := evaluateStatus(spec, 5); got != "ok" {
		t.Fatalf("5 节点集群应判定为 ok，实际 %s", got)
	}
	if got := evaluateStatus(spec, 1); got != "warning" {
		t.Fatalf("单节点集群应判定为 warning，实际 %s", got)
	}
	if got := evaluateStatus(spec, 0); got != "critical" {
		t.Fatalf("0 节点应判定为 critical，实际 %s", got)
	}
}

// TestBuildSelectorLabelContract 锁定实例与 Prometheus 标签的匹配约定。
// 该约定是「平台如何找到其他项目中间件指标」的关键：纳管信息填错就查不到数据。
func TestBuildSelectorLabelContract(t *testing.T) {
	// 显式 job + instance：精确匹配
	selector := buildSelector(Target{Job: "my-redis-job", Instance: "10.0.0.11:6379"}, "middleware-exporter")
	if selector != `job="my-redis-job",instance="10.0.0.11:6379"` {
		t.Fatalf("显式 job/instance 匹配串不符: %s", selector)
	}

	// 未填 job：按 <前缀>-<类型> 兜底
	selector = buildSelector(Target{MWType: "redis", Name: "redis-dev-01"}, "middleware-exporter")
	if selector != `job="middleware-exporter-redis",instance_name="redis-dev-01"` {
		t.Fatalf("默认 job 兜底匹配串不符: %s", selector)
	}

	// 未填 job 且未配置前缀：只能按实例名匹配
	selector = buildSelector(Target{MWType: "redis", Name: "redis-dev-01"}, "")
	if selector != `instance_name="redis-dev-01"` {
		t.Fatalf("无前缀时匹配串不符: %s", selector)
	}

	// 完全未填：不应产生空标签（否则 PromQL 会退化为匹配全部序列）
	selector = buildSelector(Target{MWType: "redis"}, "middleware-exporter")
	if selector != `job="middleware-exporter-redis"` {
		t.Fatalf("仅类型时匹配串不符: %s", selector)
	}
}
