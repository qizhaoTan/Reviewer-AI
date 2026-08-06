// Package logging 提供项目统一的日志入口，基于标准库 log/slog 封装。
package log

import (
	"io"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/lmittmann/tint"
)

// Options 配置全局 logger 的行为。
type Options struct {
	// Level 控制最低输出级别，默认 slog.LevelInfo。
	Level slog.Level
	// Format 控制输出格式，默认 FormatText。
	Format Format
	// Output 是日志写入目标，默认 os.Stderr。
	Output io.Writer
}

// Format 是日志输出格式。
type Format int

const (
	// FormatText 以人类可读的文本格式输出。
	FormatText Format = iota
	// FormatJSON 以结构化 JSON 格式输出。
	FormatJSON
)

var logger atomic.Pointer[slog.Logger]

func init() {
	logger.Store(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// Init 根据 opts 配置全局 logger。未设置的字段使用默认值。
// Init 可重复调用，后续调用会替换之前的全局 logger。
func Init(opts Options) {
	output := opts.Output
	if output == nil {
		output = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{Level: opts.Level}

	var handler slog.Handler
	switch opts.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(output, handlerOpts)
	default:
		handler = slog.NewTextHandler(output, handlerOpts)
	}

	logger.Store(slog.New(handler))
}

func InitDebug() {
	handler := tint.NewTextHandler(os.Stdout, &tint.Options{
		Level:      slog.LevelDebug, // 设置日志级别
		AddSource:  true,            // 显示文件名和行号
		TimeFormat: "15:04:05.000",  // 自定义时间格式
	})
	logger.Store(slog.New(handler))
}

func Debug(msg string, args ...any) {
	logger.Load().Debug(msg, args...)
}

func Info(msg string, args ...any) {
	logger.Load().Info(msg, args...)
}

func Warn(msg string, args ...any) {
	logger.Load().Warn(msg, args...)
}

func Error(msg string, args ...any) {
	logger.Load().Error(msg, args...)
}
