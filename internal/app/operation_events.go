package app

import (
	"context"
	"fmt"
)

// OperationEvent is one progress event or warning of a user-facing operation
// (history backfill, media backfill, media retry). Human is the stderr line
// the operation prints when events are off ("" for events-only lifecycle
// events), including its trailing newline.
type OperationEvent struct {
	Event string
	Data  map[string]any
	Human string
}

type operationEventsKey struct{}

// WithOperationEvents routes the operation events emitted under ctx to sink
// instead of this process's event stream. A running `sync --follow` uses it to
// stream the events of an operation it runs for another wacli process back to
// that process, which prints them as if it had run the operation itself.
// Events of the sync run itself (history_sync, progress, ...) are not routed.
func WithOperationEvents(ctx context.Context, sink func(OperationEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, operationEventsKey{}, sink)
}

func operationEventSink(ctx context.Context) func(OperationEvent) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(operationEventsKey{}).(func(OperationEvent))
	return sink
}

// opEmitOrPrint is emitOrPrint for an operation event.
func (a *App) opEmitOrPrint(ctx context.Context, event string, data map[string]any, format string, args ...any) {
	if sink := operationEventSink(ctx); sink != nil {
		sink(OperationEvent{Event: event, Data: data, Human: fmt.Sprintf(format, args...)})
		return
	}
	a.emitOrPrint(event, data, format, args...)
}

// opEmitWarning is emitWarning for an operation warning.
func (a *App) opEmitWarning(ctx context.Context, code, message string, data map[string]any) {
	if sink := operationEventSink(ctx); sink != nil {
		if data == nil {
			data = map[string]any{}
		}
		data["code"] = code
		data["message"] = message
		sink(OperationEvent{Event: "warning", Data: data, Human: message + "\n"})
		return
	}
	a.emitWarning(code, message, data)
}

// opEmitEvent is emitEvent for an events-only operation event.
func (a *App) opEmitEvent(ctx context.Context, event string, data map[string]any) {
	if sink := operationEventSink(ctx); sink != nil {
		sink(OperationEvent{Event: event, Data: data})
		return
	}
	a.emitEvent(event, data)
}
