package cmd

import (
	"testing"
)

// TestQuoteDefault 覆盖 tableCreateFailed.log 中出现的真实默认值，
// 确保 MySQL 裸值被正确补上单引号(时间/枚举)，而数值与表达式保持原样。
func TestQuoteDefault(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		def      string
		want     string
	}{
		// 时间类型：裸值必须加引号，否则 PG 会当成算术表达式 1990-02-02 解析
		{"timestamp literal", "timestamp", "1990-02-02 11:00:00", "'1990-02-02 11:00:00'"},
		{"timestamp zero date", "timestamp", "0000-01-01 00:00:00", "'0000-01-01 00:00:00'"},
		{"datetime literal", "datetime", "0000-00-00 00:00:00", "'0000-00-00 00:00:00'"},
		// 表达式默认值不能被引号包裹，否则退化成字符串字面量
		{"current_timestamp unchanged", "timestamp", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"current_timestamp precision", "timestamp", "CURRENT_TIMESTAMP(3)", "CURRENT_TIMESTAMP(3)"},
		{"current_date unchanged", "date", "CURRENT_DATE", "CURRENT_DATE"},
		{"case insensitive expr", "timestamp", "current_timestamp", "current_timestamp"},

		// 枚举默认值：MySQL 存的是裸标识符，必须加引号
		{"enum single letter", "enum", "N", "'N'"},
		{"enum keyword", "enum", "temporary", "'temporary'"},
		{"enum numeric-looking", "enum", "1", "'1'"},
		{"enum chinese", "enum", "评审专家", "'评审专家'"},
		{"set member", "set", "a,b", "'a,b'"},

		// 数值类型保持原样
		{"int zero", "int", "0", "0"},
		{"int positive", "int", "1", "1"},
		{"double negative", "double", "-1", "-1"},
		{"decimal", "decimal", "0.00", "0.00"},
		{"bigint", "bigint", "0", "0"},

		// 字符串类型沿用原有行为
		{"varchar numeric-looking", "varchar", "0", "'0'"},
		{"char numeric-looking", "char", "1", "'1'"},
		{"varchar hex color", "varchar", "#000000", "'#000000'"},

		// 值内的单引号按 SQL 标准双写转义
		{"varchar with quote", "varchar", "O'Brien", "'O''Brien'"},
		{"enum with quote", "enum", "it's", "'it''s'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteDefault(tt.dataType, tt.def); got != tt.want {
				t.Errorf("quoteDefault(%q, %q) = %q, want %q", tt.dataType, tt.def, got, tt.want)
			}
		})
	}
}

// TestIsTimeType 时间类型判定。两类调用方传入的大小写不同，
// 且判定结果同时决定「默认值是否丢弃」「not null 是否放宽」两件事。
func TestIsTimeType(t *testing.T) {
	tests := []struct {
		dataType string
		want     bool
	}{
		{"timestamp", true},
		{"datetime", true},
		{"date", true},
		{"time", true},
		{"TIMESTAMP", true},
		{"DATETIME", true},
		{"DATE", true},
		{"DateTime", true},

		{"int", false},
		{"varchar", false},
		{"text", false},
		{"enum", false},
		{"decimal", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.dataType, func(t *testing.T) {
			if got := isTimeType(tt.dataType); got != tt.want {
				t.Errorf("isTimeType(%q) = %v, want %v", tt.dataType, got, tt.want)
			}
		})
	}
}

// TestIsZeroDatetime 覆盖 MySQL 零值日期的识别：这类默认值必须被丢弃，
// 否则 PostgreSQL 建表时会报 invalid input syntax for type timestamp。
func TestIsZeroDatetime(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		def      string
		want     bool
	}{
		// 日志中 table_file_info / mysql_login_logs 的真实取值
		{"zero month and day", "timestamp", "0000-00-00 00:00:00", true},
		{"zero year only", "timestamp", "0000-01-01 00:00:00", true},
		{"date only", "date", "0000-00-00", true},
		{"datetime with fraction", "datetime", "0000-00-00 00:00:00.000000", true},
		{"surrounding spaces", "timestamp", " 0000-00-00 00:00:00 ", true},

		// 正常值不应被误判
		{"valid datetime", "timestamp", "1990-02-02 11:00:00", false},
		{"valid date", "date", "2024-01-01", false},
		{"current_timestamp", "timestamp", "CURRENT_TIMESTAMP", false},

		// 类型守卫：非时间类型不参与零值日期判定
		{"varchar leading 0000", "varchar", "0000-abc", false},
		{"char leading 0000", "char", "0000", false},

		// 行迁移路径：类型名来自驱动 DatabaseTypeName()，都是大写
		{"uppercase DATETIME", "DATETIME", "0000-00-00 00:00:00", true},
		{"uppercase TIMESTAMP", "TIMESTAMP", "0000-00-00 00:00:00.000000", true},
		{"uppercase DATE", "DATE", "0000-00-00", true},
		{"uppercase valid value", "DATETIME", "2024-01-01 00:00:00", false},
		{"uppercase non-time type", "VARCHAR", "0000-abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isZeroDatetime(tt.dataType, tt.def); got != tt.want {
				t.Errorf("isZeroDatetime(%q, %q) = %v, want %v", tt.dataType, tt.def, got, tt.want)
			}
		})
	}
}
