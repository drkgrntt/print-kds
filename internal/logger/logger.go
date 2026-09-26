// Package logger provides structured logging for the app, wrapping a single
// package-level slog.Logger so call sites don't each construct their own.
package logger

import (
	"log/slog"
	"os"
)

var logger *slog.Logger

func init() {
	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}
	logger = slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func Info(msg string, args ...any) {
	logger.Info(msg, args...)
}

func Warn(msg string, args ...any) {
	logger.Warn(msg, args...)
}

func Debug(msg string, args ...any) {
	logger.Debug(msg, args...)
}

func Error(msg string, args ...any) {
	logger.Error(msg, args...)
}

// Fatal logs at error level and then exits the process.
func Fatal(msg string, args ...any) {
	logger.Error(msg, args...)
	os.Exit(1)
}
