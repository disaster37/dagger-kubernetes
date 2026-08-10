package observ

import (
	"io"
	"strings"

	"github.com/sirupsen/logrus"
)

const logTimestampFormat = "2006-01-02T15:04:05.000Z07:00"

// newLogger returns a logrus logger with the given level, format, and output.
// A nil out keeps logrus's default (stderr).
func newLogger(level logrus.Level, format string, out io.Writer) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(newFormatter(format))
	logger.SetLevel(level)
	if out != nil {
		logger.SetOutput(out)
	}
	return logger
}

// newFormatter returns the logrus formatter for format: "text" (any case)
// selects the text formatter; everything else falls back to JSON (per project
// convention).
func newFormatter(format string) logrus.Formatter {
	if strings.EqualFold(format, "text") {
		return &logrus.TextFormatter{
			TimestampFormat: logTimestampFormat,
			FullTimestamp:   true,
		}
	}
	return &logrus.JSONFormatter{
		TimestampFormat: logTimestampFormat,
	}
}

// NewLogger builds a structured logrus logger. level falls back to info and
// format falls back to JSON on unrecognized values (per project convention).
// Supported formats: "json", "text".
func NewLogger(level, format string) *logrus.Logger {
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	return newLogger(lvl, format, nil)
}

// NewTestLogger returns a logrus logger that discards all output, for use in
// unit and integration tests.
func NewTestLogger() *logrus.Logger {
	return newLogger(logrus.DebugLevel, "json", io.Discard)
}
