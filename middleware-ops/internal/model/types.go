package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// JSONMap 是 map[string]any 的 GORM 序列化封装。
//
// 使用 text 列而非 JSONB，便于在 PostgreSQL / 其他方言之间保持一致；
// 查询语义不依赖数据库 JSON 函数，避免方言耦合。
type JSONMap map[string]any

// Value 实现 driver.Valuer。
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal json map: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner。
func (m *JSONMap) Scan(src any) error {
	if src == nil {
		*m = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("JSONMap: unsupported scan type")
	}
	if len(raw) == 0 {
		*m = nil
		return nil
	}
	out := JSONMap{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("unmarshal json map: %w", err)
	}
	*m = out
	return nil
}

// JSONMapList 是 []map[string]any 的序列化封装，用于建议列表。
type JSONMapList []map[string]any

// Value 实现 driver.Valuer。
func (l JSONMapList) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("marshal json map list: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner。
func (l *JSONMapList) Scan(src any) error {
	if src == nil {
		*l = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("JSONMapList: unsupported scan type")
	}
	if len(raw) == 0 {
		*l = nil
		return nil
	}
	out := JSONMapList{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("unmarshal json map list: %w", err)
	}
	*l = out
	return nil
}

// JSONStringSlice 是 []string 的序列化封装。
type JSONStringSlice []string

// Value 实现 driver.Valuer。
func (s JSONStringSlice) Value() (driver.Value, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal string slice: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner。
func (s *JSONStringSlice) Scan(src any) error {
	if src == nil {
		*s = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("JSONStringSlice: unsupported scan type")
	}
	if len(raw) == 0 {
		*s = nil
		return nil
	}
	out := JSONStringSlice{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("unmarshal string slice: %w", err)
	}
	*s = out
	return nil
}

// Contains 报告字符串切片是否包含 target。
func (s JSONStringSlice) Contains(target string) bool {
	for _, item := range s {
		if item == target {
			return true
		}
	}
	return false
}

// Vector 为 768 维向量字段。
//
// pgvector 构建标签下由 db.VectorType 映射为 vector(768)；否则以 JSON 文本存储，
// 检索退化为应用层余弦相似度（规模 ≤200 实例时可接受，见 10. 非功能需求）。
type Vector []float32

// Value 实现 driver.Valuer。
func (v Vector) Value() (driver.Value, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal([]float32(v))
	if err != nil {
		return nil, fmt.Errorf("marshal vector: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner，同时兼容 pgvector 返回的 "[1,2,3]" 字符串形式。
func (v *Vector) Scan(src any) error {
	if src == nil {
		*v = nil
		return nil
	}
	var raw string
	switch val := src.(type) {
	case []byte:
		raw = string(val)
	case string:
		raw = val
	default:
		return errors.New("Vector: unsupported scan type")
	}
	if raw == "" {
		*v = nil
		return nil
	}
	// pgvector 文本表示为 "[0.1,0.2]"。
	if raw[0] == '[' && (len(raw) < 2 || raw[1] != '{') {
		trimmed := []byte(raw)
		trimmed[0] = '['
		trimmed[len(trimmed)-1] = ']'
		out := Vector{}
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return fmt.Errorf("unmarshal pgvector: %w", err)
		}
		*v = out
		return nil
	}
	out := Vector{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fmt.Errorf("unmarshal vector: %w", err)
	}
	*v = out
	return nil
}
