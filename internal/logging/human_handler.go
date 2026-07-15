package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const humanTimestampLayout = "2006-01-02T15:04:05.000Z07:00"

type boundAttribute struct {
	groups []string
	attr   slog.Attr
}

type humanTextHandler struct {
	writer     io.Writer
	level      slog.Leveler
	template   string
	groups     []string
	attributes []boundAttribute
	mu         *sync.Mutex
}

func newHumanTextHandler(writer io.Writer, level slog.Leveler, template string) slog.Handler {
	return &humanTextHandler{writer: writer, level: level, template: template, mu: &sync.Mutex{}}
}

func (handler *humanTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= handler.level.Level()
}

func (handler *humanTextHandler) Handle(_ context.Context, record slog.Record) error {
	attributes := append([]boundAttribute(nil), handler.attributes...)
	record.Attrs(func(attribute slog.Attr) bool {
		attributes = append(attributes, boundAttribute{groups: append([]string(nil), handler.groups...), attr: attribute})
		return true
	})
	renderedAttributes := renderHumanAttributes(attributes)
	if renderedAttributes != "" {
		renderedAttributes = " " + renderedAttributes
	}
	line := strings.NewReplacer(
		"{time}", record.Time.Format(humanTimestampLayout),
		"{level}", record.Level.String(),
		"{message}", singleLine(record.Message),
		"{attributes}", renderedAttributes,
	).Replace(handler.template)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	_, err := io.WriteString(handler.writer, line+"\n")
	return err
}

func (handler *humanTextHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	clone := handler.clone()
	for _, attribute := range attributes {
		clone.attributes = append(clone.attributes, boundAttribute{groups: append([]string(nil), handler.groups...), attr: attribute})
	}
	return clone
}

func (handler *humanTextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return handler
	}
	clone := handler.clone()
	clone.groups = append(clone.groups, name)
	return clone
}

func (handler *humanTextHandler) clone() *humanTextHandler {
	clone := *handler
	clone.groups = append([]string(nil), handler.groups...)
	clone.attributes = append([]boundAttribute(nil), handler.attributes...)
	return &clone
}

func renderHumanAttributes(attributes []boundAttribute) string {
	parts := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		appendHumanAttribute(&parts, attribute.groups, attribute.attr)
	}
	return strings.Join(parts, " ")
}

func appendHumanAttribute(parts *[]string, groups []string, attribute slog.Attr) {
	value := attribute.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		nestedGroups := groups
		if attribute.Key != "" {
			nestedGroups = append(append([]string(nil), groups...), attribute.Key)
		}
		for _, nested := range value.Group() {
			appendHumanAttribute(parts, nestedGroups, nested)
		}
		return
	}
	keyParts := append([]string(nil), groups...)
	if attribute.Key != "" {
		keyParts = append(keyParts, attribute.Key)
	}
	if len(keyParts) == 0 {
		return
	}
	*parts = append(*parts, strings.Join(keyParts, ".")+"="+formatHumanValue(value))
}

func formatHumanValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return quoteHumanString(value.String())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		return quoteHumanString(fmt.Sprint(value.Any()))
	default:
		return quoteHumanString(value.String())
	}
}

func quoteHumanString(value string) string {
	if value == "" || strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.ContainsAny(value, `="\`) {
		return strconv.Quote(singleLine(value))
	}
	return singleLine(value)
}

func singleLine(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
