package service

import (
	"context"
	"regexp"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// AttributionService records trace ownership and enriches trace_meta from
// OTLP ingest. All operations are best-effort: errors are logged and never
// returned, so ingest never breaks telemetry forwarding.
type AttributionService struct {
	projects      *ProjectService
	groups        domain.GroupRepository
	orgs          domain.OrgRepository
	traceMeta     domain.TraceMetaRepository
	logger        *logrus.Logger
	projectMapper *ProjectMapper
}

// NewAttributionService returns an AttributionService.
func NewAttributionService(projects *ProjectService, groups domain.GroupRepository, traceMeta domain.TraceMetaRepository, logger *logrus.Logger) *AttributionService {
	return &AttributionService{projects: projects, groups: groups, traceMeta: traceMeta, logger: logger}
}

// SetProjectMapper wires the optional config-driven project→group mapper. A nil
// or inactive mapper disables the feature (backward compatible). Mirrors the
// AuthService.SetOAuthRevalidator optional-dependency pattern.
func (a *AttributionService) SetProjectMapper(m *ProjectMapper) {
	a.projectMapper = m
}

// SetOrgRepository wires org slug lookups for org-scoped mapping rules.
func (a *AttributionService) SetOrgRepository(orgs domain.OrgRepository) {
	a.orgs = orgs
}

// Provision records trace_id -> user_id (plus the engine version) at engine
// provision time.
func (a *AttributionService) Provision(ctx context.Context, orgID, traceID, userID, version string) {
	if traceID == "" {
		return
	}
	if orgID == "" {
		if a.orgs == nil {
			// Legacy/test wiring without an org repository keeps the original
			// unscoped behaviour; production always wires orgs.
		} else if boot, err := a.orgs.BootstrapOrgID(ctx); err == nil {
			orgID = boot
		} else {
			a.logger.WithField("trace_id", traceID).Warn("attribution provision skipped: no organization")
			return
		}
	}
	if err := a.traceMeta.UpsertProvision(ctx, orgID, traceID, userID, version); err != nil {
		a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution provision failed")
	}
}

// maxIngestFieldLen bounds OTLP-derived string fields (ci_repo, version, ...)
// before they are persisted, so a hostile telemetry body cannot store
// unbounded values (CWE-770).
const maxIngestFieldLen = 256

// ingestDedupWindow suppresses repeated non-terminal Raft writes for the same
// trace within this window (ADR-051). Terminal transitions always write.
const ingestDedupWindow = 15 * time.Second

// Ingest enriches trace_meta from an OTLP-derived summary. When repo is
// non-empty it upserts the project, resolves the group (explicit assignment
// wins; otherwise regex auto-assign by group id order), and writes the row.
func (a *AttributionService) Ingest(ctx context.Context, orgID, traceID, userID, ciRepo, gitRemote, ciProvider, version, status string, durationMS int64, startedAt time.Time) {
	if traceID == "" || len(traceID) > maxIngestFieldLen {
		return
	}
	if orgID == "" {
		if a.orgs == nil {
			// Legacy/test wiring without an org repository keeps the original
			// unscoped behaviour; production always wires orgs.
		} else if boot, err := a.orgs.BootstrapOrgID(ctx); err == nil {
			orgID = boot
		} else {
			a.logger.WithField("trace_id", traceID).Warn("attribution ingest skipped: no organization")
			return
		}
	}
	// The pipeline identity is the CI repo slug when the run happens in CI,
	// otherwise the git remote reported by the Dagger CLI (local runs emit
	// "dagger.io/git.remote" but not "dagger.io/ci.repo").
	repo := ciRepo
	if repo == "" {
		repo = gitRemote
	}
	// All fields below come from client-supplied OTLP span data; bound them
	// before persistence.
	if len(repo) > maxIngestFieldLen {
		repo = ""
	}
	if len(ciProvider) > maxIngestFieldLen {
		ciProvider = ""
	}
	if len(version) > maxIngestFieldLen {
		version = ""
	}
	if len(status) > maxIngestFieldLen {
		status = ""
	}

	var groupID string
	if repo != "" {
		proj, err := a.projects.GetOrCreateByName(ctx, orgID, repo)
		if err != nil {
			a.logger.WithError(err).WithField("ci_repo", repo).Warn("attribution: project upsert failed")
		} else {
			groupID = proj.GroupID
			if groupID == "" {
				groupID = a.resolveGroup(ctx, proj)
			}
		}
	}

	now := time.Now().UTC()
	if a.ingestUnchanged(ctx, traceID, groupID, repo, status, now) {
		return
	}

	m := &domain.TraceMeta{
		TraceID:     traceID,
		OrgID:       orgID,
		UserID:      userID,
		GroupID:     groupID,
		ProjectName: repo,
		Status:      status,
		Version:     version,
		CIProvider:  ciProvider,
		CIRepo:      repo,
		DurationMS:  durationMS,
		StartedAt:   startedAt,
		UpdatedAt:   now,
	}
	if err := a.traceMeta.UpsertIngest(ctx, m); err != nil {
		a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution ingest failed")
	}
}

// ingestUnchanged reports whether a non-terminal ingest would be a no-op: the
// stored row already has the same status/group/repo and was updated within the
// dedup window. Terminal statuses are never deduplicated.
func (a *AttributionService) ingestUnchanged(ctx context.Context, traceID, groupID, repo, status string, now time.Time) bool {
	if isTerminalStatus(status) {
		return false
	}
	existing, err := a.traceMeta.Get(ctx, traceID)
	if err != nil || existing == nil {
		return false
	}
	if isTerminalStatus(existing.Status) {
		return false
	}
	if existing.Status != status || existing.GroupID != groupID || existing.CIRepo != repo {
		return false
	}
	return now.Sub(existing.UpdatedAt) < ingestDedupWindow
}

// isTerminalStatus reports whether a trace status is final.
func isTerminalStatus(status string) bool {
	return status == "success" || status == "failed"
}

// MarkFailed transitions a non-terminal trace to "failed" with reason.
// Best-effort like Provision/Ingest: errors are logged, never returned to
// break the caller. Returns transitioned=true when the status changed.
func (a *AttributionService) MarkFailed(ctx context.Context, traceID, reason string) bool {
	if traceID == "" || len(traceID) > maxIngestFieldLen {
		return false
	}
	if len(reason) > maxIngestFieldLen {
		reason = reason[:maxIngestFieldLen]
	}
	transitioned, err := a.traceMeta.MarkFailed(ctx, traceID, reason)
	if err != nil {
		a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution mark failed")
		return false
	}
	return transitioned
}

// resolveGroup assigns proj to a group via, in order: config project_mappings
// (first-match-wins, authoritative when matched), then per-group
// AutoAssignPattern. Returns the group id, or "" when nothing matches.
func (a *AttributionService) resolveGroup(ctx context.Context, proj *domain.Project) string {
	if a.projectMapper.Active() {
		if groupID, matched := a.mappingAssign(ctx, proj); matched {
			return groupID
		}
	}
	return a.autoAssign(ctx, proj)
}

// mappingAssign resolves proj.Name against projectMapper. Returns (groupID,
// matched). matched=false only when no rule matched (caller falls through to
// autoAssign). matched=true with empty groupID means a rule matched but the
// target group is missing (skipped; caller must NOT fall through).
func (a *AttributionService) mappingAssign(ctx context.Context, proj *domain.Project) (string, bool) {
	orgName := ""
	if a.orgs != nil {
		if o, err := a.orgs.Get(ctx, proj.OrgID); err == nil {
			orgName = o.Name
		}
	}
	name, matched := a.projectMapper.MapForOrg(proj.OrgID, orgName, proj.Name)
	if !matched {
		return "", false
	}
	g, err := a.groups.GetByName(ctx, proj.OrgID, name)
	if err != nil {
		a.logger.WithError(err).WithFields(logrus.Fields{
			"project_id": proj.ID,
			"project":    proj.Name,
			"group":      name,
		}).Warn("attribution: project_mappings target group not found, skipping")
		return "", true
	}
	if _, err := a.projects.Assign(ctx, proj.OrgID, proj.ID, g.ID); err != nil {
		// The trace is still attributed; only the persistence failed.
		a.logger.WithError(err).WithField("project_id", proj.ID).Warn("attribution: project_mappings persist failed")
	}
	a.logger.WithFields(logrus.Fields{
		"project_id": proj.ID,
		"project":    proj.Name,
		"group":      name,
		"group_id":   g.ID,
	}).Info("attribution: project_mappings assigned")
	return g.ID, true
}

// autoAssign finds the first group (by id order) whose AutoAssignPattern
// matches the project name, persists the assignment, and returns the group
// id. Invalid patterns are skipped with a warning. Returns "" when no match.
func (a *AttributionService) autoAssign(ctx context.Context, proj *domain.Project) string {
	gs, err := a.groups.ListByOrg(ctx, proj.OrgID)
	if err != nil {
		a.logger.WithError(err).Warn("auto-assign: list groups failed")
		return ""
	}
	// Stable order by group id (first match wins).
	sort.Slice(gs, func(i, j int) bool { return gs[i].ID < gs[j].ID })
	for _, g := range gs {
		if g.AutoAssignPattern == "" {
			continue
		}
		re, err := regexp.Compile(g.AutoAssignPattern)
		if err != nil {
			a.logger.WithError(err).WithField("group_id", g.ID).Warn("auto-assign: invalid pattern")
			continue
		}
		if re.MatchString(proj.Name) {
			if _, err := a.projects.Assign(ctx, proj.OrgID, proj.ID, g.ID); err != nil {
				// The trace is still attributed; only the persistence failed.
				a.logger.WithError(err).WithField("project_id", proj.ID).Warn("auto-assign: persist failed")
			}
			return g.ID
		}
	}
	return ""
}
