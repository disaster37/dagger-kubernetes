package observ

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

func TestNewLoggerLevels(t *testing.T) {
	tests := []struct {
		in   string
		want logrus.Level
	}{
		{"debug", logrus.DebugLevel},
		{"info", logrus.InfoLevel},
		{"warn", logrus.WarnLevel},
		{"error", logrus.ErrorLevel},
	}

	for _, tt := range tests {
		l := NewLogger(tt.in)
		if l == nil {
			t.Fatalf("NewLogger(%q) returned nil", tt.in)
		}
		if l.GetLevel() != tt.want {
			t.Fatalf("NewLogger(%q) level = %v, want %v", tt.in, l.GetLevel(), tt.want)
		}
	}
}

func TestNewLoggerInvalidLevelFallback(t *testing.T) {
	l := NewLogger("bogus-level")
	if l.GetLevel() != logrus.InfoLevel {
		t.Fatalf("expected fallback to InfoLevel, got %v", l.GetLevel())
	}
}

func TestNewTestLogger(t *testing.T) {
	l := NewTestLogger()
	if l == nil {
		t.Fatal("nil test logger")
	}
	if l.GetLevel() != logrus.DebugLevel {
		t.Fatalf("expected DebugLevel, got %v", l.GetLevel())
	}
	// Should not panic when logging.
	l.Info("discarded")
}

func TestNewMetricsRegisters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("nil metrics")
	}

	m.EngineAcquireTotal.WithLabelValues("v0.21.4", "request").Inc()
	m.EngineAcquireDuration.WithLabelValues("v0.21.4").Observe(0.1)
	m.ActiveLeases.Inc()
	m.ActiveReplicas.WithLabelValues("v0.21.4").Set(1)
	m.OTelIngestTotal.WithLabelValues("traces", "success").Inc()

	fam, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(fam) == 0 {
		t.Fatal("no metric families gathered")
	}
}

func TestNewMetricsNilRegistry(t *testing.T) {
	m := NewMetrics(nil)
	// Counters must still be usable without registration (no panic).
	m.EngineAcquireTotal.WithLabelValues("v0.21.4", "request").Inc()
	m.ActiveLeases.Inc()
}
