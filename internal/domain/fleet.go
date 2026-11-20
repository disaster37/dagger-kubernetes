package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
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
	Org           string    `json:"org,omitempty"`      // org.ID
	OrgSlug       string    `json:"org_slug,omitempty"` // org.Name (display)
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

// PVCInfo is a per-pod PVC of an engine version's StatefulSet.
type PVCInfo struct {
	Name    string
	Ordinal int
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
	// ListPVCs returns the per-pod PVCs of the version's StatefulSet.
	// Each entry carries the PVC name and its ordinal (extracted from the
	// StatefulSet volume-claim naming convention). PVCs whose ordinal
	// cannot be parsed are omitted.
	ListPVCs(version string) ([]PVCInfo, error)
	// DeletePVC deletes a single PVC by name. A missing PVC (already
	// deleted) is treated as success (idempotent).
	DeletePVC(name string) error
}

// FleetScope identifies one deployed engine fleet: a version scoped to an org
// (orgSlug == "" for shared/unscoped fleets when fleet.isolation=shared).
type FleetScope struct {
	OrgID   string
	Version string
}

// maxEngineStsNameLen keeps the pod label controller-revision-hash
// (<stsName>-<10charHash>) within Kubernetes's 63-char label limit.
const maxEngineStsNameLen = 52

// EngineStsName returns the canonical StatefulSet name for an engine fleet.
// org is the organization storage ID ("org-<16hex>"; "" for a shared fleet).
// Build-metadata versions that would exceed maxEngineStsNameLen are compacted
// deterministically with a SHA-256 suffix over (org, version) so neither orgs
// nor build-metadata versions can collide.
func EngineStsName(org, version string) string {
	suffix := VersionSlug(version)
	if org != "" {
		suffix = fmt.Sprintf("%s-%s", VersionSlug(org), suffix)
	}
	const prefix = "dagger-engine-"
	if len(prefix)+len(suffix) <= maxEngineStsNameLen {
		return fmt.Sprintf("%s%s", prefix, suffix)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s", org, version)))
	budget := maxEngineStsNameLen - len(prefix) - 1 - 8
	trimmed := strings.TrimRight(suffix[:budget], "-")
	return fmt.Sprintf("%s%s-%s", prefix, trimmed, hex.EncodeToString(sum[:])[:8])
}

// EnginePodName returns the ordinal pod name for an engine fleet.
func EnginePodName(org, version string, ordinal int) string {
	return fmt.Sprintf("%s-%d", EngineStsName(org, version), ordinal)
}

// EngineServiceName returns the headless service name for an engine fleet,
// which by design matches the StatefulSet name.
func EngineServiceName(org, version string) string {
	return EngineStsName(org, version)
}

// EnginePodRegex returns a PromQL-safe regex matching the pod names of a
// version's engine fleet in either isolation mode: the shared
// "dagger-engine-<vslug>-N" and the per-org "dagger-engine-<orgslug>-<vslug>-N".
// The version slug contains only [A-Za-z0-9_-] (no regex metacharacters), so
// the result is safe to interpolate into a {pod=~"..."} matcher.
func EnginePodRegex(version string) string {
	return fmt.Sprintf("dagger-engine-(?:[a-z0-9-]+-)?%s", VersionSlug(version))
}

// EngineOrgPodRegex returns a PromQL-safe regex matching only the pods of an
// org-scoped engine fleet ("dagger-engine-<orgslug>-<vslug>-N"). The engine
// metric templates append the "-.*" ordinal suffix, so this returns just the
// org-scoped StatefulSet name pattern. The org slug is server-assigned, but it
// is still QuoteMeta-escaped so a hostile value cannot inject regex
// metacharacters; the caller additionally escapes the PromQL label value.
func EngineOrgPodRegex(org, version string) string {
	return regexp.QuoteMeta(EngineStsName(org, version))
}

// StsName returns the shared (unscoped) StatefulSet name for a version.
func StsName(version string) string {
	return EngineStsName("", version)
}

// OrgStsName returns the per-org StatefulSet name used when
// fleet.isolation=per_org.
func OrgStsName(org, version string) string {
	return EngineStsName(org, version)
}

// PodName returns the shared (unscoped) ordinal pod name for a version.
func PodName(version string, ordinal int) string {
	return EnginePodName("", version, ordinal)
}

// ServiceName returns the shared (unscoped) headless service name, which by
// design matches the StatefulSet name.
func ServiceName(version string) string {
	return EngineServiceName("", version)
}

// OrgServiceName returns the per-org headless service name.
func OrgServiceName(org, version string) string {
	return EngineServiceName(org, version)
}

// ScopedFleetProvider is implemented by providers that can scope their
// StatefulSets/Services/PVCs to an organization (fleet.isolation=per_org) and
// enumerate the actually-deployed scopes.
type ScopedFleetProvider interface {
	WithScope(orgID string) FleetProvider
	// AllScopes returns one FleetScope per deployed engine StatefulSet so the
	// sweeper can clean up every org's fleet (including leftovers from config
	// toggles or orgs since removed from Raft).
	AllScopes() ([]FleetScope, error)
}
