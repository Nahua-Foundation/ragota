// Package logger предоставляет глобальный zerolog-логгер для всего приложения.
//
// Инициализация — InitLogger(), должна вызываться один раз на старте.
// До инициализации логгер пишет в stderr в console-формате с уровнем debug.
//
// Использование: logger.Log().Info().Msg("starting indexing")
package logger

import (
	"io"
	"os"

	"github.com/rs/zerolog"
)

var log zerolog.Logger

func init() {
	log = zerolog.New(os.Stderr).With().Timestamp().Logger()
}

// InitLogger настраивает глобальный логгер.
func InitLogger(level string, jsonFormat bool, out io.Writer) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	if out == nil {
		out = os.Stderr
	}
	if jsonFormat {
		log = zerolog.New(out).Level(lvl).With().Timestamp().Logger()
	} else {
		log = zerolog.New(zerolog.ConsoleWriter{Out: out, TimeFormat: "15:04:05"}).Level(lvl).With().Timestamp().Logger()
	}
}

// OpenLogFile открывает лог-файл в .ragota/log/app.log (относительно root).
// Возвращает *os.File (caller должен закрыть) и путь.
func OpenLogFile(root string) (*os.File, string, error) {
	if err := os.MkdirAll(root+"/.ragota/log", 0o755); err != nil {
		return nil, "", err
	}
	path := root + "/.ragota/log/app.log"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// Log возвращает глобальный логгер.
func Log() *zerolog.Logger {
	return &log
}
