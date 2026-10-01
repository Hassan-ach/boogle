package utils

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorGreen  = "\033[32m"
	colorGray   = "\033[90m"
)

func colorForLevel(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return colorGray
	case slog.LevelInfo:
		return colorGreen
	case slog.LevelWarn:
		return colorYellow
	case slog.LevelError:
		return colorRed
	default:
		return colorBlue
	}
}

type SimpleTextHandler struct {
	attrs []slog.Attr
}

func (h *SimpleTextHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *SimpleTextHandler) Handle(_ context.Context, r slog.Record) error {
	levelColor := colorForLevel(r.Level)
	level := r.Level.String()
	msg := r.Message

	var attrStr strings.Builder
	for _, a := range h.attrs {
		fmt.Fprintf(&attrStr, " %s=%v", a.Key, a.Value)
	}

	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&attrStr, " %s=%v", a.Key, a.Value)
		return true
	})

	_, _ = fmt.Fprintf(os.Stdout, "%s%s%s %q%s\n",
		levelColor, level, colorReset, msg, attrStr.String(),
	)
	return nil
}

func (h *SimpleTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &SimpleTextHandler{attrs: newAttrs}
}

func (h *SimpleTextHandler) WithGroup(_ string) slog.Handler { return h }

type MultiHandler struct {
	handlers []slog.Handler
}

func NewMultiLogger(fileName string) *Logger {
	txtHandler := &SimpleTextHandler{}

	file, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open log file: %v", err)
	}
	jsonHandler := slog.NewJSONHandler(file, nil)

	logger := slog.New(&MultiHandler{[]slog.Handler{txtHandler, jsonHandler}})
	return &Logger{logger, file}
}

func (l *Logger) Close() {
	if l.file != nil {
		_ = l.file.Close()
	}
}

func NewDiscardLogger() *Logger {
	return &Logger{Logger: slog.New(slog.DiscardHandler)}
}

func (m MultiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if !h.Enabled(ctx, l) {
			return false
		}
	}
	return true
}

func (m MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		err := h.Handle(ctx, r)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{newHandlers}
}

func (m MultiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{newHandlers}
}

type Logger struct {
	*slog.Logger
	file *os.File
}
