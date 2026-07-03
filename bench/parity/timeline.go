package parity

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Point is one datapath-gauge sample. Bytes counters are cumulative within
// the owning cell (each cell has a fresh registry).
type Point struct {
	Ms              int64   `json:"ms"` // since cell start
	Inflight        float64 `json:"inflight"`
	Window          float64 `json:"window"`
	QueueDepth      float64 `json:"queue_depth"`
	GoodputBps      float64 `json:"goodput_bps"`
	Uploads         float64 `json:"uploads"`
	UploadedBytes   float64 `json:"uploaded_bytes"`
	RemoteReads     float64 `json:"remote_reads"`
	RemoteReadBytes float64 `json:"remote_read_bytes"`
}

// Timeline is the sampled gauge history of one dittofs cell.
type Timeline struct {
	IntervalMs int64   `json:"interval_ms"`
	Points     []Point `json:"points"`
}

// sampler polls a prometheus registry and records datapath Points.
type sampler struct {
	reg      *prometheus.Registry
	interval time.Duration
	start    time.Time

	mu     sync.Mutex
	points []Point
	cancel context.CancelFunc
	done   chan struct{}
}

func newSampler(reg *prometheus.Registry, interval time.Duration) *sampler {
	return &sampler{reg: reg, interval: interval}
}

func (s *sampler) run(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	s.start = time.Now()
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sample()
			}
		}
	}()
}

// stop takes a final sample and returns the recorded timeline.
func (s *sampler) stop() Timeline {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	return Timeline{IntervalMs: s.interval.Milliseconds(), Points: s.points}
}

func (s *sampler) sample() {
	families, err := s.reg.Gather()
	if err != nil {
		return
	}
	p := Point{Ms: time.Since(s.start).Milliseconds()}
	for _, mf := range families {
		switch mf.GetName() {
		case "dittofs_datapath_uploads_inflight":
			p.Inflight = firstGauge(mf)
		case "dittofs_datapath_upload_window":
			p.Window = firstGauge(mf)
		case "dittofs_datapath_upload_queue_depth":
			p.QueueDepth = firstGauge(mf)
		case "dittofs_datapath_upload_goodput_bytes_per_second":
			p.GoodputBps = firstGauge(mf)
		case "dittofs_datapath_uploads_total":
			for _, m := range mf.GetMetric() {
				p.Uploads += m.GetCounter().GetValue()
			}
		case "dittofs_datapath_upload_bytes":
			for _, m := range mf.GetMetric() {
				p.UploadedBytes += m.GetHistogram().GetSampleSum()
			}
		case "dittofs_datapath_block_range_reads_total":
			p.RemoteReads = firstCounter(mf)
		case "dittofs_datapath_block_range_read_bytes_total":
			p.RemoteReadBytes = firstCounter(mf)
		}
	}
	s.mu.Lock()
	s.points = append(s.points, p)
	s.mu.Unlock()
}

func firstGauge(mf *dto.MetricFamily) float64 {
	if ms := mf.GetMetric(); len(ms) > 0 {
		return ms[0].GetGauge().GetValue()
	}
	return 0
}

func firstCounter(mf *dto.MetricFamily) float64 {
	if ms := mf.GetMetric(); len(ms) > 0 {
		return ms[0].GetCounter().GetValue()
	}
	return 0
}
