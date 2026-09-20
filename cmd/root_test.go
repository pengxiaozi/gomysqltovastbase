package cmd

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/lib/pq"
)

// TestIsRetryableConnErr 判定哪些错误值得重试。
// 核心原则：只重试连接类故障，SQL 层错误重试多少次结果都一样，
// 重试它们只会浪费时间并掩盖真正的 SQL 问题。
func TestIsRetryableConnErr(t *testing.T) {
	// 复现真实遇到的那条错误：连接被对端强制关闭
	// {"Op":"read","Net":"tcp",...,"Err":{"Syscall":"wsarecv","Err":10054}}
	connReset := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "wsarecv", Err: syscall.ECONNRESET},
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not retryable", nil, false},

		// 连接类故障 —— 应当重试
		{"raw conn reset", syscall.ECONNRESET, true},
		{"wrapped conn reset", connReset, true},
		{"wrapped with fmt", fmt.Errorf("exec failed: %w", connReset), true},
		{"broken pipe", syscall.EPIPE, true},
		{"conn refused", syscall.ECONNREFUSED, true},
		{"plain EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"driver bad conn", driver.ErrBadConn, true},
		{
			name: "server shutting down",
			err:  &pq.Error{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"},
			want: true,
		},
		{
			name: "server closed the connection",
			err:  errors.New("pq: server closed the connection unexpectedly"),
			want: true,
		},

		// SQL 层错误 —— 不应重试
		{
			name: "not-null violation",
			err:  &pq.Error{Code: "23502", Message: `null value in column "x" violates not-null constraint`},
			want: false,
		},
		{
			name: "syntax error",
			err:  &pq.Error{Code: "42601", Message: `syntax error at or near "null"`},
			want: false,
		},
		{
			name: "undefined column",
			err:  &pq.Error{Code: "42703", Message: `column "userName" does not exist`},
			want: false,
		},
		{
			name: "string too long",
			err:  &pq.Error{Code: "22001", Message: "value too long for type character varying(20)"},
			want: false,
		},
		{"generic error", errors.New("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableConnErr(tt.err); got != tt.want {
				t.Errorf("isRetryableConnErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestErrSummary 覆盖 failedTable.log 的错误摘要格式。
// 目标是每张失败的表现在都能在日志里一眼看出原因，
// 而不是只列个表名、还要去 errorTableData.log 里翻。
func TestErrSummary(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "pg error keeps code and message",
			err: &pq.Error{
				Severity: "ERROR",
				Code:     "23502",
				Message:  `null value in column "createtime" violates not-null constraint`,
				Detail:   "Failing row contains (178750, 0, 35001, ...).",
			},
			want: `23502 null value in column "createtime" violates not-null constraint`,
		},
		{
			// 没有错误码时回退到 err.Error()，lib/pq 会在消息前加 "pq: " 前缀
			name: "pg error without code falls back to Error()",
			err: &pq.Error{
				Severity: "ERROR",
				Message:  "syntax error",
			},
			want: "pq: syntax error",
		},
		{
			name: "plain error is collapsed to one line",
			err:  errors.New("first line\nsecond line"),
			want: "first line second line",
		},
		{
			name: "wrapped pg error still matched",
			err:  errors.Join(errors.New("context"), &pq.Error{Code: "22007", Message: "invalid input syntax"}),
			want: "22007 invalid input syntax",
		},
		{
			name: "driver bad conn error",
			err:  sql.ErrNoRows,
			want: "sql: no rows in result set",
		},
		{
			name: "nil error yields empty string",
			err:  nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errSummary(tt.err); got != tt.want {
				t.Errorf("errSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
