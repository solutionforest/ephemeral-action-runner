package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

type fanoutHandler []slog.Handler

func (handler fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, child := range handler {
		if child.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (handler fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var handlerErrors []error
	for _, child := range handler {
		if child.Enabled(ctx, record.Level) {
			if err := child.Handle(ctx, record.Clone()); err != nil {
				handlerErrors = append(handlerErrors, err)
			}
		}
	}
	return errors.Join(handlerErrors...)
}

func (handler fanoutHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	children := make(fanoutHandler, len(handler))
	for index, child := range handler {
		children[index] = child.WithAttrs(attributes)
	}
	return children
}

func (handler fanoutHandler) WithGroup(name string) slog.Handler {
	children := make(fanoutHandler, len(handler))
	for index, child := range handler {
		children[index] = child.WithGroup(name)
	}
	return children
}

type levelRangeHandler struct {
	handler slog.Handler
	min     slog.Leveler
	max     *slog.Level
}

func (handler levelRangeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < handler.min.Level() || handler.max != nil && level >= *handler.max {
		return false
	}
	return handler.handler.Enabled(ctx, level)
}

func (handler levelRangeHandler) Handle(ctx context.Context, record slog.Record) error {
	if !handler.Enabled(ctx, record.Level) {
		return nil
	}
	return handler.handler.Handle(ctx, record)
}

func (handler levelRangeHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	handler.handler = handler.handler.WithAttrs(attributes)
	return handler
}

func (handler levelRangeHandler) WithGroup(name string) slog.Handler {
	handler.handler = handler.handler.WithGroup(name)
	return handler
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

type warningHandler struct {
	handler slog.Handler
	state   *warningState
}

type warningState struct {
	once   sync.Once
	writer io.Writer
}

func (handler warningHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.handler.Enabled(ctx, level)
}

func (handler warningHandler) Handle(ctx context.Context, record slog.Record) error {
	err := handler.handler.Handle(ctx, record)
	if err != nil {
		handler.state.once.Do(func() {
			_, _ = fmt.Fprintf(handler.state.writer, "warning: logging sink write failed: %v\n", err)
		})
	}
	return err
}

func (handler warningHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	handler.handler = handler.handler.WithAttrs(attributes)
	return handler
}

func (handler warningHandler) WithGroup(name string) slog.Handler {
	handler.handler = handler.handler.WithGroup(name)
	return handler
}

func formattedHandler(writer io.Writer, format Format, level slog.Leveler) slog.Handler {
	options := &slog.HandlerOptions{Level: level}
	if format == FormatJSON {
		return slog.NewJSONHandler(writer, options)
	}
	return slog.NewTextHandler(writer, options)
}

func managerHandler(options Options, file io.Writer) slog.Handler {
	children := make(fanoutHandler, 0, 3)
	if options.ManagerSinks.Has(SinkConsole) {
		warning := slog.LevelWarn
		stdout := managerConsoleHandler(options.Stdout, options)
		stderr := managerConsoleHandler(options.Stderr, options)
		children = append(children,
			levelRangeHandler{handler: stdout, min: options.Level, max: &warning},
			levelRangeHandler{handler: stderr, min: warning},
		)
	}
	if options.ManagerSinks.Has(SinkFile) {
		children = append(children, formattedHandler(file, options.ManagerFileFormat, options.Level))
	}
	if len(children) == 0 {
		return discardHandler{}
	}
	return warningHandler{handler: children, state: &warningState{writer: options.Stderr}}
}

func managerConsoleHandler(writer io.Writer, options Options) slog.Handler {
	if options.ManagerConsoleFormat == FormatJSON {
		return formattedHandler(writer, FormatJSON, options.Level)
	}
	return newHumanTextHandler(writer, options.Level, options.ManagerConsoleTextFormat)
}
