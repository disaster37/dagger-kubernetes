package observ

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RaftMetrics holds Prometheus metrics for Raft cluster health.
type RaftMetrics struct {
	LeaderChanges          prometheus.Counter
	FollowerHeartbeatAgeMs *prometheus.GaugeVec
	AppliedIndexDelta      *prometheus.GaugeVec
	RaftState              prometheus.Gauge
	LastLogIndex           prometheus.Gauge
	CommitIndex            prometheus.Gauge
	AppliedIndex           prometheus.Gauge
	NumPeers               prometheus.Gauge
	JoinAttempts           prometheus.Counter
	JoinFailures           prometheus.Counter
	DNSResolutionFailures  prometheus.Counter
	LeadershipTransfers    prometheus.Counter
	DeadServersDetected    prometheus.Counter
}

// NewRaftMetrics creates and registers Raft metrics.
func NewRaftMetrics() *RaftMetrics {
	return &RaftMetrics{
		LeaderChanges: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_leader_changes_total",
			Help: "Total number of leader changes observed.",
		}),
		FollowerHeartbeatAgeMs: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "raft_follower_heartbeat_age_ms",
			Help: "Milliseconds since last heartbeat from each follower.",
		}, []string{"follower_id"}),
		AppliedIndexDelta: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "raft_applied_index_delta",
			Help: "Difference between leader and follower applied index.",
		}, []string{"follower_id"}),
		RaftState: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_state",
			Help: "Current Raft state (0=Follower, 1=Candidate, 2=Leader).",
		}),
		LastLogIndex: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_last_log_index",
			Help: "Last log index.",
		}),
		CommitIndex: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_commit_index",
			Help: "Commit index.",
		}),
		AppliedIndex: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_applied_index",
			Help: "Applied index.",
		}),
		NumPeers: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_num_peers",
			Help: "Number of peers in the cluster.",
		}),
		JoinAttempts: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_join_attempts_total",
			Help: "Total number of join attempts.",
		}),
		JoinFailures: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_join_failures_total",
			Help: "Total number of failed join attempts.",
		}),
		DNSResolutionFailures: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_dns_resolution_failures_total",
			Help: "Total number of DNS resolution failures.",
		}),
		LeadershipTransfers: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_leadership_transfers_total",
			Help: "Total number of leadership transfers.",
		}),
		DeadServersDetected: promauto.NewCounter(prometheus.CounterOpts{
			Name: "raft_dead_servers_detected_total",
			Help: "Total number of dead servers detected by autopilot.",
		}),
	}
}

// UpdateFromStats updates metrics from raft.Stats().
func (m *RaftMetrics) UpdateFromStats(stats map[string]string) {
	if state, ok := stats["state"]; ok {
		var val float64
		switch state {
		case "Follower":
			val = 0
		case "Candidate":
			val = 1
		case "Leader":
			val = 2
		}
		m.RaftState.Set(val)
	}
	if v, ok := stats["last_log_index"]; ok {
		m.LastLogIndex.Set(parseFloat64(v))
	}
	if v, ok := stats["commit_index"]; ok {
		m.CommitIndex.Set(parseFloat64(v))
	}
	if v, ok := stats["applied_index"]; ok {
		m.AppliedIndex.Set(parseFloat64(v))
	}
	if v, ok := stats["num_peers"]; ok {
		m.NumPeers.Set(parseFloat64(v))
	}
}

// parseFloat64 parses a string to float64, returning 0 on error.
func parseFloat64(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// UpdateFollowerHeartbeat updates follower heartbeat age metric.
func (m *RaftMetrics) UpdateFollowerHeartbeat(followerID string, ageMs float64) {
	m.FollowerHeartbeatAgeMs.WithLabelValues(followerID).Set(ageMs)
}

// UpdateAppliedIndexDelta updates follower applied index delta metric.
func (m *RaftMetrics) UpdateAppliedIndexDelta(followerID string, delta float64) {
	m.AppliedIndexDelta.WithLabelValues(followerID).Set(delta)
}
