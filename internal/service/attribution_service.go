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
	projects  *ProjectService
	groups    domain.GroupRepository
	traceMeta domain.TraceMetaRepository
	logger    *logrus.Logger
}

// NewAttributionService returns an AttributionService.
func NewAttributionService(projects *ProjectService, groups domain.GroupRepository, traceMeta domain.TraceMetaRepository, logger *logrus.Logger) *AttributionService {
	return &AttributionService{projects: projects, groups: groups, traceMeta: traceMeta, logger: logger}
}

// Provision records trace_id -> user_id (plus the engine version) at engine
// provision time.
func (a *AttributionService) Provision(ctx context.Context, traceID, userID, version string) {
	if traceID == "" {
		return
	}
	if err := a.traceMeta.UpsertProvision(ctx, traceID, userID, version); err != nil {
		a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution provision failed")
	}
}

// maxIngestFieldLen bounds OTLP-derived string fields (ci_repo, version, ...)
// before they are persisted, so a hostile telemetry body cannot store
// unbounded values (CWE-770).
const maxIngestFieldLen = 256

// Ingest enriches trace_meta from an OTLP-derived summary. When repo is
// non-empty it upserts the project, resolves the group (explicit assignment
// wins; otherwise regex auto-assign by group id order), and writes the row.
func (a *AttributionService) Ingest(ctx context.Context, traceID, userID, ciRepo, gitRemote, ciProvider, version, status string, durationMS int64, startedAt time.Time) {
	if traceID == "" || len(traceID) > maxIngestFieldLen {
		return
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
		proj, err := a.projects.GetOrCreateByName(ctx, repo)
		if err != nil {
			a.logger.WithError(err).WithField("ci_repo", repo).Warn("attribution: project upsert failed")
		} else {
			groupID = proj.GroupID
			if groupID == "" {
				groupID = a.autoAssign(ctx, proj)
			}
		}
	}

	m := &domain.TraceMeta{
		TraceID:     traceID,
		UserID:      userID,
		GroupID:     groupID,
		ProjectName: repo,
		Status:      status,
		Version:     version,
		CIProvider:  ciProvider,
		CIRepo:      repo,
		DurationMS:  durationMS,
		StartedAt:   startedAt,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := a.traceMeta.UpsertIngest(ctx, m); err != nil {
		a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution ingest failed")
	}
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

// autoAssign finds the first group (by id order) whose AutoAssignPattern
// matches the project name, persists the assignment, and returns the group
// id. Invalid patterns are skipped with a warning. Returns "" when no match.
func (a *AttributionService) autoAssign(ctx context.Context, proj *domain.Project) string {
	gs, err := a.groups.List(ctx)
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
			if _, err := a.projects.Assign(ctx, proj.ID, g.ID); err != nil {
				// The trace is still attributed; only the persistence failed.
				a.logger.WithError(err).WithField("project_id", proj.ID).Warn("auto-assign: persist failed")
			}
			return g.ID
		}
	}
	return ""
}
