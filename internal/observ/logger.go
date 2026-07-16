package observ

import (
	"io"

	"github.com/sirupsen/logrus"
)

// NewLogger builds a structured JSON logrus logger.
// An unrecognized level falls back to InfoLevel (per project convention).
func NewLogger(level string) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	})

	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	logger.SetLevel(lvl)

	return logger
}

// NewTestLogger returns a logrus logger that discards all output, for use in
// unit and integration tests.
func NewTestLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	})
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.DebugLevel)
	return logger
}
