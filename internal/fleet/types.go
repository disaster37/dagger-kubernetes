package fleet

import "time"

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

type Provider interface {
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
