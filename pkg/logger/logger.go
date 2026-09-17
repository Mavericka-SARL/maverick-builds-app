package logger

import (
	"os"

	"github.com/rs/zerolog"
)

func New(service string) zerolog.Logger {
	level := zerolog.InfoLevel
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = zerolog.DebugLevel
	}

	return zerolog.New(os.Stdout).
		Level(level).
		With().
		Timestamp().
		Str("service", service).
		Logger()
}
