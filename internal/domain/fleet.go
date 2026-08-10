package domain

import (
	"fmt"
	"strings"
	"time"
)

type Replica struct {
	Name           string
	Ordinal        int
	Version        string
	PodIP          string
	Ready          bool
	StartedAt      time.Time
	PinnedSessions int
}

type FleetInfo struct {
	Version       string
	STSName       string
	Replicas      int
	ReadyReplicas int
	Ordinals      []Replica
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
