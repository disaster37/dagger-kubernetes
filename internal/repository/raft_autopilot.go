package repository

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/sirupsen/logrus"
)

// AutopilotConfig controls the autopilot subsystem behavior.
type AutopilotConfig struct {
	// Enabled enables the autopilot subsystem.
	Enabled bool
	// CleanupDeadServers enables automatic removal of dead servers.
	CleanupDeadServers bool
	// DeadServerLastContactThreshold is how long since last contact before
	// a server is considered dead. Default: 24h.
	DeadServerLastContactThreshold time.Duration
	// MinQuorum is the minimum number of voters before autopilot stops
	// removing servers. Default: 2 (for 3-node clusters).
	MinQuorum int
	// StabilizationTime is how long to wait after a membership change
	// before allowing another change. Default: 10s.
	StabilizationTime time.Duration
	// HeartbeatInterval is how often the leader checks follower heartbeats.
	// Default: 1s.
	HeartbeatInterval time.Duration
}

// DefaultAutopilotConfig returns the default autopilot configuration.
func DefaultAutopilotConfig() AutopilotConfig {
	return AutopilotConfig{
		Enabled:                        true,
		CleanupDeadServers:             true,
		DeadServerLastContactThreshold: 24 * time.Hour,
		MinQuorum:                      2,
		StabilizationTime:              10 * time.Second,
		HeartbeatInterval:              1 * time.Second,
	}
}

// FollowerState tracks the health of a single follower.
type FollowerState struct {
	ID                raft.ServerID
	Address           raft.ServerAddress
	LastHeartbeat     time.Time
	LastHeartbeatAck  time.Time
	AppliedIndexDelta int64
	Term              uint64
	Healthy           bool
}

// Autopilot manages Raft cluster health, dead server detection, and stabilization.
type Autopilot struct {
	cfg    AutopilotConfig
	r      *raft.Raft
	logger *logrus.Logger

	mu         sync.RWMutex
	followers  map[raft.ServerID]*FollowerState
	lastChange time.Time
	leaderCh   <-chan bool
}

// NewAutopilot creates a new Autopilot instance.
func NewAutopilot(cfg AutopilotConfig, r *raft.Raft, leaderCh <-chan bool, logger *logrus.Logger) *Autopilot {
	return &Autopilot{
		cfg:       cfg,
		r:         r,
		logger:    logger,
		followers: make(map[raft.ServerID]*FollowerState),
		leaderCh:  leaderCh,
	}
}

// Start begins the autopilot control loop. Blocks until ctx is cancelled.
func (a *Autopilot) Start(ctx context.Context) {
	if !a.cfg.Enabled {
		return
	}

	heartbeatTicker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	cleanupTicker := time.NewTicker(30 * time.Second)
	defer cleanupTicker.Stop()

	isLeader := false

	for {
		select {
		case <-ctx.Done():
			return
		case leader, ok := <-a.leaderCh:
			if !ok {
				return
			}
			isLeader = leader
			if isLeader {
				a.logger.Info("autopilot: became leader")
				a.stabilize()
			}
		case <-heartbeatTicker.C:
			if !isLeader {
				continue
			}
			a.heartbeatTracker(ctx)
		case <-cleanupTicker.C:
			if !isLeader || !a.cfg.CleanupDeadServers {
				continue
			}
			if err := a.RemoveDeadServers(ctx); err != nil {
				a.logger.WithError(err).Warn("autopilot: failed to remove dead servers")
			}
		}
	}
}

// GetFollowerState returns the state for a specific follower.
func (a *Autopilot) GetFollowerState(id raft.ServerID) (*FollowerState, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state, ok := a.followers[id]
	return state, ok
}

// GetAllFollowerStates returns all tracked follower states.
func (a *Autopilot) GetAllFollowerStates() map[raft.ServerID]*FollowerState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make(map[raft.ServerID]*FollowerState, len(a.followers))
	for k, v := range a.followers {
		result[k] = v
	}
	return result
}

// DeadServers returns servers that exceed the dead-server threshold.
func (a *Autopilot) DeadServers() []raft.ServerID {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var dead []raft.ServerID
	for id, state := range a.followers {
		if a.isDead(state) {
			dead = append(dead, id)
		}
	}
	return dead
}

// RemoveDeadServers removes all dead servers from the cluster configuration.
// Respects MinQuorum and StabilizationTime.
func (a *Autopilot) RemoveDeadServers(ctx context.Context) error {
	// Get current voter count BEFORE acquiring lock (Raft call may block).
	cfgFuture := a.r.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return fmt.Errorf("get configuration: %w", err)
	}
	currentVoters := 0
	for _, srv := range cfgFuture.Configuration().Servers {
		if srv.Suffrage == raft.Voter {
			currentVoters++
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	dead := a.deadServersLocked()
	if len(dead) == 0 {
		return nil
	}

	if currentVoters-len(dead) < a.cfg.MinQuorum {
		a.logger.WithFields(logrus.Fields{
			"current_voters": currentVoters,
			"dead":           len(dead),
			"min_quorum":     a.cfg.MinQuorum,
		}).Warn("autopilot: refusing to remove dead servers, would drop below min_quorum")
		return nil
	}

	// Check stabilization period.
	if time.Since(a.lastChange) < a.cfg.StabilizationTime {
		return nil
	}

	for _, id := range dead {
		a.logger.WithField("server_id", string(id)).Info("autopilot: removing dead server")
		future := a.r.RemoveServer(id, 0, 0)
		if err := future.Error(); err != nil {
			a.logger.WithError(err).WithField("server_id", string(id)).Warn("autopilot: failed to remove dead server")
			continue
		}
		delete(a.followers, id)
	}

	a.lastChange = time.Now()
	return nil
}

// deadServersLocked returns dead server IDs. Must hold mu.Lock.
func (a *Autopilot) deadServersLocked() []raft.ServerID {
	var dead []raft.ServerID
	for id, state := range a.followers {
		if a.isDead(state) {
			dead = append(dead, id)
		}
	}
	return dead
}

// stabilize waits for the stabilization period before allowing changes.
func (a *Autopilot) stabilize() {
	a.mu.Lock()
	a.lastChange = time.Now()
	a.mu.Unlock()
}

// heartbeatTracker runs on the leader, tracking follower presence.
// Servers present in the current Raft configuration get fresh timestamps.
// Servers that disappear from the configuration keep their old timestamps
// and will be detected as dead after DeadServerLastContactThreshold.
func (a *Autopilot) heartbeatTracker(ctx context.Context) {
	// Get current configuration to find known servers.
	cfgFuture := a.r.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return
	}
	config := cfgFuture.Configuration()

	// Build a set of voter IDs currently in the configuration.
	currentVoters := make(map[raft.ServerID]bool, len(config.Servers))
	for _, srv := range config.Servers {
		if srv.Suffrage == raft.Voter {
			currentVoters[srv.ID] = true
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Update heartbeat timestamps for voters currently in the configuration.
	// New voters are added; existing voters get fresh timestamps.
	for _, srv := range config.Servers {
		if srv.Suffrage != raft.Voter {
			continue
		}
		state, exists := a.followers[srv.ID]
		if !exists {
			state = &FollowerState{
				ID:      srv.ID,
				Address: srv.Address,
				Healthy: true,
			}
			a.followers[srv.ID] = state
		}
		state.LastHeartbeat = time.Now()
		state.Healthy = true
	}

	// Mark servers that are no longer in the configuration as unhealthy.
	// Their LastHeartbeat timestamp will age, and they'll be detected as dead
	// after DeadServerLastContactThreshold.
	for id, state := range a.followers {
		if !currentVoters[id] {
			state.Healthy = false
		}
	}
}

// isDead returns true if a follower exceeds the dead-server threshold.
// A server is dead if it is marked unhealthy OR its last heartbeat exceeds
// the configured threshold.
func (a *Autopilot) isDead(state *FollowerState) bool {
	if !state.Healthy {
		return true
	}
	return time.Since(state.LastHeartbeat) > a.cfg.DeadServerLastContactThreshold
}
