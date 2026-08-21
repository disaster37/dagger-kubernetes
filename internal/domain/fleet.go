package domain

import (
	"fmt"
	"strings"
	"time"
)

type Replica struct {
	Name           string    `json:"name"`
	Ordinal        int       `json:"ordinal"`
	Version        string    `json:"version"`
	PodIP          string    `json:"podIP"`
	Ready          bool      `json:"ready"`
	StartedAt      time.Time `json:"startedAt"`
	PinnedSessions int       `json:"pinnedSessions"`
}

type FleetInfo struct {
	Version       string    `json:"version"`
	STSName       string    `json:"stsName"`
	Replicas      int       `json:"replicas"`
	ReadyReplicas int       `json:"readyReplicas"`
	Ordinals      []Replica `json:"ordinals"`
}

type AcquireResult struct {
	PodName string
	PodIP   string
	Version string
	Image   string
}

type FleetProvider interface {
	EnsureStatefulSet(version string, image string) error
	DeleteStatefulSet(version string) error
	EnsureService(version string) error
	DeleteService(version string) error
	GetReplicas(version string) ([]Replica, error)
	ScaleUp(version string, targetReplicas int) error
	ScaleDown(version string, ordinal int) error
	GetReadyReplicaIP(version string, podName string) (string, error)
	WaitForReady(version string, podName string) error
	GetEngineImage(version string) string
	AllVersions() ([]string, error)
	// VersionIdleSince returns the RFC3339 UTC time the version was first
	// observed idle (zero replicas, no pinned sessions), or ok=false when the
	// StatefulSet is missing or carries no idle-since annotation. A malformed
	// annotation returns ok=true with a parse error.
	VersionIdleSince(version string) (time.Time, bool, error)
	// SetVersionIdleSince stamps (or, when idleSince is the zero time, clears)
	// the idle-since annotation on the version's StatefulSet.
	SetVersionIdleSince(version string, idleSince time.Time) error
}

func VersionSlug(version string) string {
	return strings.ReplaceAll(version, ".", "-")
}

func StsName(version string) string {
	return fmt.Sprintf("dagger-engine-%s", VersionSlug(version))
}

func PodName(version string, ordinal int) string {
	return fmt.Sprintf("dagger-engine-%s-%d", VersionSlug(version), ordinal)
}

// ServiceName returns the headless service name, which by design matches the
// StatefulSet name.
func ServiceName(version string) string {
	return StsName(version)
}
