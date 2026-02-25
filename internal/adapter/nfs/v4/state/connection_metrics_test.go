package state

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

func TestConnectionMetrics_NilSafe(t *testing.T) {
	// All methods on a nil *ConnectionMetrics must not panic.
	var m *ConnectionMetrics

	m.RecordBind("fore")
	m.RecordBind("back")
	m.RecordBind("both")
	m.RecordUnbind("explicit")
	m.RecordUnbind("disconnect")
	m.RecordUnbind("session_destroy")
	m.RecordUnbind("reaper")
	m.SetBoundConnections("abc123", 5)
	m.RemoveSessionGauge("abc123")
}

func TestConnectionMetrics_RecordBind(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewConnectionMetrics(reg)

	m.RecordBind("fore")
	m.RecordBind("fore")
	m.RecordBind("back")
	m.RecordBind("both")

	// Verify fore counter = 2
	foreVal := counterValue(t, m.BindTotal, "fore")
	if foreVal != 2 {
		t.Errorf("BindTotal{direction=fore} = %f, want 2", foreVal)
	}

	// Verify back counter = 1
	backVal := counterValue(t, m.BindTotal, "back")
	if backVal != 1 {
		t.Errorf("BindTotal{direction=back} = %f, want 1", backVal)
	}

	// Verify both counter = 1
	bothVal := counterValue(t, m.BindTotal, "both")
	if bothVal != 1 {
		t.Errorf("BindTotal{direction=both} = %f, want 1", bothVal)
	}
}

func TestConnectionMetrics_RecordUnbind(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewConnectionMetrics(reg)

	m.RecordUnbind("explicit")
	m.RecordUnbind("disconnect")
	m.RecordUnbind("disconnect")
	m.RecordUnbind("session_destroy")
	m.RecordUnbind("reaper")

	explicitVal := counterValue(t, m.UnbindTotal, "explicit")
	if explicitVal != 1 {
		t.Errorf("UnbindTotal{reason=explicit} = %f, want 1", explicitVal)
	}

	disconnectVal := counterValue(t, m.UnbindTotal, "disconnect")
	if disconnectVal != 2 {
		t.Errorf("UnbindTotal{reason=disconnect} = %f, want 2", disconnectVal)
	}

	sessionDestroyVal := counterValue(t, m.UnbindTotal, "session_destroy")
	if sessionDestroyVal != 1 {
		t.Errorf("UnbindTotal{reason=session_destroy} = %f, want 1", sessionDestroyVal)
	}

	reaperVal := counterValue(t, m.UnbindTotal, "reaper")
	if reaperVal != 1 {
		t.Errorf("UnbindTotal{reason=reaper} = %f, want 1", reaperVal)
	}
}

func TestConnectionMetrics_BoundGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewConnectionMetrics(reg)

	m.SetBoundConnections("session-aaa", 3)
	m.SetBoundConnections("session-bbb", 7)

	aaaVal := gaugeValue(t, m.BoundGauge, "session-aaa")
	if aaaVal != 3 {
		t.Errorf("BoundGauge{session_id=session-aaa} = %f, want 3", aaaVal)
	}

	bbbVal := gaugeValue(t, m.BoundGauge, "session-bbb")
	if bbbVal != 7 {
		t.Errorf("BoundGauge{session_id=session-bbb} = %f, want 7", bbbVal)
	}

	// Update the gauge
	m.SetBoundConnections("session-aaa", 1)
	aaaVal2 := gaugeValue(t, m.BoundGauge, "session-aaa")
	if aaaVal2 != 1 {
		t.Errorf("BoundGauge{session_id=session-aaa} after update = %f, want 1", aaaVal2)
	}

	// Remove session gauge
	m.RemoveSessionGauge("session-aaa")
	// After removal, the metric should no longer have the label
}

// counterValue extracts the value from a CounterVec for the given label.
func counterValue(t *testing.T, cv *prometheus.CounterVec, label string) float64 {
	t.Helper()
	counter, err := cv.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", label, err)
	}
	var metric io_prometheus_client.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}

// gaugeValue extracts the value from a GaugeVec for the given label.
func gaugeValue(t *testing.T, gv *prometheus.GaugeVec, label string) float64 {
	t.Helper()
	gauge, err := gv.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", label, err)
	}
	var metric io_prometheus_client.Metric
	if err := gauge.Write(&metric); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}
