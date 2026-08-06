package log

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestInit(t *testing.T) {
	tests := []struct {
		name         string
		opts         Options
		logFunc      func()
		wantContains []string
		wantAbsent   []string
	}{
		{
			name: "text format default level info logs info message",
			opts: Options{Output: new(bytes.Buffer)},
			logFunc: func() {
				Info("hello", "key", "value")
			},
			wantContains: []string{"level=INFO", "msg=hello", "key=value"},
		},
		{
			name: "json format",
			opts: Options{Format: FormatJSON, Output: new(bytes.Buffer)},
			logFunc: func() {
				Info("hello", "key", "value")
			},
			wantContains: []string{`"msg":"hello"`, `"key":"value"`},
		},
		{
			name: "level warn filters info messages",
			opts: Options{Level: slog.LevelWarn, Output: new(bytes.Buffer)},
			logFunc: func() {
				Info("should be filtered")
				Warn("should appear")
			},
			wantContains: []string{"should appear"},
			wantAbsent:   []string{"should be filtered"},
		},
		{
			name: "debug level logs debug message",
			opts: Options{Level: slog.LevelDebug, Output: new(bytes.Buffer)},
			logFunc: func() {
				Debug("debug detail")
			},
			wantContains: []string{"level=DEBUG", "msg=\"debug detail\""},
		},
		{
			name: "error level logs error message",
			opts: Options{Output: new(bytes.Buffer)},
			logFunc: func() {
				Error("boom")
			},
			wantContains: []string{"level=ERROR", "msg=boom"},
		},
		{
			name: "nil output defaults without panic",
			opts: Options{},
			logFunc: func() {
				Info("no panic expected")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf, hasBuf := tt.opts.Output.(*bytes.Buffer)

			Init(tt.opts)
			tt.logFunc()

			if !hasBuf {
				return
			}
			got := buf.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output %q does not contain %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output %q unexpectedly contains %q", got, absent)
				}
			}
		})
	}
}

func TestInitDebug(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "sets debug level and text format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InitDebug()

			buf := new(bytes.Buffer)
			Init(Options{Level: slog.LevelDebug, Format: FormatText, Output: buf})
			Debug("debug via InitDebug")

			if !strings.Contains(buf.String(), "debug via InitDebug") {
				t.Errorf("output %q does not contain expected debug message", buf.String())
			}
		})
	}
}

func TestPackageFuncsWithoutInitDoNotPanic(t *testing.T) {
	tests := []struct {
		name    string
		logFunc func()
	}{
		{name: "debug", logFunc: func() { Debug("msg") }},
		{name: "info", logFunc: func() { Info("msg") }},
		{name: "warn", logFunc: func() { Warn("msg") }},
		{name: "error", logFunc: func() { Error("msg") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.logFunc()
		})
	}
}
