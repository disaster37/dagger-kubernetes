package observ

import (
	"io"

	"github.com/sirupsen/logrus"
)

const logTimestampFormat = "2006-01-02T15:04:05.000Z07:00"

// newJSONLogger returns a logrus logger with the project's JSON formatter and
// the given level and output. A nil out keeps logrus's default (stderr).
func newJSONLogger(level logrus.Level, out io.Writer) *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: logTimestampFormat,
	})
	logger.SetLevel(level)
	if out != nil {
		logger.SetOutput(out)
	}
	return logger
}

// NewLogger builds a structured JSON logrus logger.
// An unrecognized level falls back to InfoLevel (per project convention).
func NewLogger(level string) *logrus.Logger {
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	return newJSONLogger(lvl, nil)
}

// NewTestLogger returns a logrus logger that discards all output, for use in
// unit and integration tests.
func NewTestLogger() *logrus.Logger {
	return newJSONLogger(logrus.DebugLevel, io.Discard)
}
