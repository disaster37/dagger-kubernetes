package service

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	statusProbeTimeout = 5 * time.Second
	statusCacheTTL     = 5 * time.Second
)

// StatusService implements domain.StatusProvider. It probes each platform
// service (supervisor, cache, telemetry backends, fleet) and rolls the results
// into a PlatformStatus. The last result is cached for statusCacheTTL so kube
// liveness/readiness probes do not trigger probe storms.
type StatusService struct {
	cfg          *domain.Config
	cache        *Cache
	router       *RegistryRouter // may be nil (s3 backend)
	fleetManager *Manager        // may be nil
	logger       *logrus.Logger

	mu       sync.Mutex
	cached   *domain.PlatformStatus
	cachedAt time.Time
}

func NewStatusService(cfg *domain.Config, cache *Cache, router *RegistryRouter, fleet *Manager, logger *logrus.Logger) *StatusService {
	return &StatusService{
		cfg:          cfg,
		cache:        cache,
		router:       router,
		fleetManager: fleet,
		logger:       logger,
	}
}

// Status implements domain.StatusProvider. It never returns an error unless
// the context is cancelled; it always returns a status payload so the UI can
// render even when every dependency is down.
func (s *StatusService) Status(ctx context.Context) (*domain.PlatformStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && time.Since(s.cachedAt) < statusCacheTTL {
		return s.cached, nil
	}

	status := s.probe(ctx)
	s.cached = status
	s.cachedAt = time.Now()
	return status, nil
}

func (s *StatusService) probe(ctx context.Context) *domain.PlatformStatus {
	supervisor := newServiceStatus("supervisor", "control", true)
	supervisor.State = domain.ServiceOK

	services := []domain.ServiceStatus{
		supervisor,
		s.probeCache(ctx),
		s.probeTelemetry(ctx, "collector", s.cfg.Telemetry.CollectorURL),
		s.probeTelemetry(ctx, "tempo", s.cfg.Telemetry.TempoURL),
		s.probeTelemetry(ctx, "loki", s.cfg.Telemetry.LokiURL),
		s.probeTelemetry(ctx, "victoria", s.cfg.Telemetry.VictoriaURL),
		s.probeFleet(),
	}

	return &domain.PlatformStatus{
		State:     rollup(services),
		Services:  services,
		CheckedAt: rfc3339(time.Now()),
	}
}

// newServiceStatus builds a status row stamped with the current check time.
func newServiceStatus(name, category string, configured bool) domain.ServiceStatus {
	return domain.ServiceStatus{
		Name:       name,
		Category:   category,
		Configured: configured,
		CheckedAt:  rfc3339(time.Now()),
	}
}

// probeCache checks the cache backend (registry ping or s3 bucket presence).
func (s *StatusService) probeCache(ctx context.Context) domain.ServiceStatus {
	st := newServiceStatus("cache", "cache", s.cache != nil && s.cache.Type != "")
	if !st.Configured {
		st.State = domain.ServiceUnknown
		return st
	}
	switch s.cache.Type {
	case "registry":
		if s.router == nil || len(s.router.Backends()) == 0 {
			st.State = domain.ServiceDown
			st.Message = "registry not configured"
			return st
		}
		var down []string
		up := 0
		for _, b := range s.router.Backends() {
			client, ok := s.router.ClientByID(b.ID)
			if !ok || client.Ping(ctx) != nil {
				down = append(down, b.ID)
				continue
			}
			up++
		}
		switch {
		case up == 0:
			st.State = domain.ServiceDown
			st.Message = "registry unreachable"
		case len(down) > 0:
			st.State = domain.ServiceDegraded
			st.Message = fmt.Sprintf("registry backend unreachable: %s", strings.Join(down, ", "))
		default:
			st.State = domain.ServiceOK
		}
		return st
	case "s3":
		if s.cache.S3.Bucket == "" {
			st.State = domain.ServiceDown
			st.Message = "s3 bucket not configured"
			return st
		}
		st.State = domain.ServiceOK
		return st
	default:
		st.State = domain.ServiceDown
		st.Message = "unknown cache backend"
		return st
	}
}

// probeTelemetry dials the service's host:port.
func (s *StatusService) probeTelemetry(ctx context.Context, name, rawURL string) domain.ServiceStatus {
	st := newServiceStatus(name, "telemetry", rawURL != "")
	if !st.Configured {
		st.State = domain.ServiceUnknown
		return st
	}
	probeCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	if err := probeTCP(probeCtx, rawURL); err != nil {
		st.State = domain.ServiceDown
		st.Message = "unreachable"
		return st
	}
	st.State = domain.ServiceOK
	return st
}

// probeFleet reports fleet health from the engine StatefulSet manager.
func (s *StatusService) probeFleet() domain.ServiceStatus {
	st := newServiceStatus("fleet", "fleet", true)
	if s.fleetManager == nil {
		st.State = domain.ServiceOK
		return st
	}
	infos, err := s.fleetManager.AllFleetInfo()
	if err != nil {
		st.State = domain.ServiceDown
		st.Message = "fleet provider unavailable"
		return st
	}
	for _, info := range infos {
		if info.ReadyReplicas < info.Replicas {
			st.State = domain.ServiceDegraded
			st.Message = "some engine replicas not ready"
			return st
		}
	}
	st.State = domain.ServiceOK
	return st
}

// probeTCP dials the host:port of a URL, defaulting the port from the scheme.
func probeTCP(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Host
	if host == "" {
		host = u.Path
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(host, port)
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// rollup computes the aggregate state: down if any configured service is down;
// else degraded if any configured service is degraded; else ok. Unconfigured
// (unknown) services do not affect the rollup.
func rollup(services []domain.ServiceStatus) domain.ServiceState {
	state := domain.ServiceOK
	for _, svc := range services {
		if !svc.Configured {
			continue
		}
		switch svc.State {
		case domain.ServiceDown:
			return domain.ServiceDown
		case domain.ServiceDegraded:
			state = domain.ServiceDegraded
		}
	}
	return state
}
