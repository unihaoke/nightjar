package cache

import (
	"strconv"
)

// fmtInt 把整数格式化为字符串。
//
// 独立成函数便于集中处理进制与错误边界。
func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }

// fmtSscan 解析整数字符串。
func fmtSscan(s string, out *int64) (int, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	*out = v
	return 1, nil
}
