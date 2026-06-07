package logger

import (
	"ragota/pkg/state"

	"github.com/rs/zerolog"
)

// TUIHook — zerolog хук, который перехватывает warn/error логи
// и передаёт их в Bus для отображения в TUI.
type TUIHook struct {
	bus *state.Bus
}

// NewTUIHook создаёт хук для перехвата логов в TUI.
func NewTUIHook(bus *state.Bus) *TUIHook {
	return &TUIHook{bus: bus}
}

// Run реализует zerolog.Hook интерфейс.
func (h *TUIHook) Run(e *zerolog.Event, level zerolog.Level, message string) {
	// Перехватываем только warn и error
	if level < zerolog.WarnLevel {
		return
	}

	// Определяем уровень
	levelStr := "warn"
	if level >= zerolog.ErrorLevel {
		levelStr = "error"
	}

	h.bus.AddLog(levelStr, message)
}
