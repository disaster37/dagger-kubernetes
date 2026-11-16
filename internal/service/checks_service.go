package service

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// LoadReportInput is the check-runner load-phase report.
type LoadReportInput struct {
	Checks        []string          `json:"checks"`
	WorkflowFiles map[string]string `json:"workflow_files,omitempty"`
	ContextFiles  map[string][]byte `json:"context_files,omitempty"`
}

// CompleteReportInput is the check-runner completion report.
type CompleteReportInput struct {
	Checks []domain.CheckItem `json:"checks"`
	Error  string             `json:"error,omitempty"`
}

// ChecksService verifies forge webhooks and dispatches Cloud Checks runs.
type ChecksService struct {
	sources   domain.ForgeSourceRepository
	secrets   *SecretService
	projects  *ProjectService
	orgs      *OrgService
	runner    domain.CheckJobRunner
	checkRuns domain.CheckRunRepository
	workflows *WorkflowService
	providers map[domain.ForgeKind]domain.ForgeProvider
	publicURL string
	logger    *logrus.Logger
}

// NewChecksService returns a ChecksService.
func NewChecksService(sources domain.ForgeSourceRepository, secrets *SecretService, projects *ProjectService, orgs *OrgService, runner domain.CheckJobRunner, checkRuns domain.CheckRunRepository, workflows *WorkflowService, providers []domain.ForgeProvider, publicURL string, logger *logrus.Logger) *ChecksService {
	m := make(map[domain.ForgeKind]domain.ForgeProvider, len(providers))
	for _, p := range providers {
		m[p.Kind()] = p
	}
	return &ChecksService{
		sources: sources, secrets: secrets, projects: projects, orgs: orgs, runner: runner,
		checkRuns: checkRuns, workflows: workflows, providers: m, publicURL: publicURL, logger: logger,
	}
}

// HandleWebhook verifies and dispatches a forge webhook. HMAC failures return
// ErrForbidden and must never be recorded as token failures (shared NAT IPs).
func (s *ChecksService) HandleWebhook(ctx context.Context, provider domain.ForgeKind, sourceID string, headers map[string]string, body []byte) (*domain.WebhookEvent, error) {
	src, err := s.sources.Get(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if src.Provider != provider {
		return nil, fmt.Errorf("%w: provider mismatch", domain.ErrValidation)
	}
	p, ok := s.providers[provider]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported forge %q", domain.ErrValidation, provider)
	}
	secret, err := s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.WebhookSecretCiphertext)
	if err != nil {
		return nil, fmt.Errorf("open webhook secret: %w", err)
	}
	if !p.VerifyWebhook(src, secret, headers, body) {
		return nil, fmt.Errorf("%w: invalid webhook signature", domain.ErrForbidden)
	}
	ev, err := domain.ParseForgeWebhook(provider, forgeEventName(provider, headers), body)
	if err != nil {
		return nil, err
	}
	if ev.Repo == "" {
		ev.Repo = src.Repo
	}
	proj, err := s.projects.GetOrCreateByName(ctx, ev.Repo)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	record := &domain.CheckRunRecord{
		ID:            fmt.Sprintf("check-%s-%s", sourceID, ev.SHA),
		OrgID:         src.OrgID,
		ProjectID:     proj.ID,
		SourceID:      src.ID,
		Provider:      provider,
		Repo:          ev.Repo,
		Event:         ev.Event,
		SHA:           ev.SHA,
		Ref:           ev.Ref,
		DefaultBranch: ev.DefaultBranch,
		TargetBranch:  ev.TargetBranch,
		Tag:           ev.Tag,
		PRNumber:      ev.PRNumber,
		IsFork:        ev.IsFork,
		IsRelease:     ev.Event == "release",
		Status:        "running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if ev.IsFork {
		record.Status = "waiting_approval"
	}
	if s.checkRuns != nil {
		if err := s.checkRuns.Upsert(ctx, record); err != nil {
			return nil, err
		}
	}
	s.reportToForge(ctx, src, &domain.CheckRun{
		Name: "dagger/checks", HeadSHA: ev.SHA, State: domain.CheckPending,
		Title: "Dagger checks queued", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
	})
	if ev.IsFork {
		s.logger.WithFields(logrus.Fields{
			"source_id": src.ID,
			"sha":       ev.SHA,
			"pr":        ev.PRNumber,
		}).Info("fork PR check run held for approval")
		return ev, nil
	}
	if err := s.dispatchCheckJob(ctx, src, record); err != nil {
		return ev, err
	}
	return ev, nil
}

// dispatchCheckJob resolves credentials/secrets and launches the check Job.
func (s *ChecksService) dispatchCheckJob(ctx context.Context, src *domain.ForgeSource, record *domain.CheckRunRecord) error {
	if s.runner == nil {
		return nil
	}
	token, err := s.orgs.EnsureToken(ctx, src.OrgID)
	if err != nil {
		return fmt.Errorf("ensure org token: %w", err)
	}
	cloneToken, err := s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.TokenCiphertext)
	if err != nil {
		return fmt.Errorf("open clone token: %w", err)
	}
	secrets := map[string]string{}
	if !record.IsFork || record.IncludeSecrets {
		secrets, err = s.secrets.ResolveAllForCheck(ctx, src.OrgID, record.ProjectID)
		if err != nil {
			return fmt.Errorf("resolve check secrets: %w", err)
		}
	}
	if err := s.runner.CancelSuperseded(ctx, record.ProjectID, record.Ref); err != nil {
		s.logger.WithError(err).Warn("cancel superseded check runs failed")
	}
	spec := &domain.CheckRunSpec{
		ID: record.ID, OrgID: record.OrgID, ProjectID: record.ProjectID, SourceID: record.SourceID,
		Provider: record.Provider, Repo: record.Repo, SHA: record.SHA, Ref: record.Ref,
		DefaultBranch: record.DefaultBranch, TargetBranch: record.TargetBranch, Tag: record.Tag,
		PRNumber: record.PRNumber, IsFork: record.IsFork, IsRelease: record.IsRelease,
		Event: record.Event, Token: token, CloneToken: cloneToken, Secrets: secrets,
	}
	return s.runner.Run(ctx, spec)
}

// RecordLoad stores the discovered checks/workflows and starts pre-check workflows.
func (s *ChecksService) RecordLoad(ctx context.Context, callerOrgID, runID string, in LoadReportInput) (*domain.CheckRunRecord, error) {
	record, err := s.checkRuns.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if record.OrgID != callerOrgID {
		return nil, fmt.Errorf("%w: check run belongs to another org", domain.ErrForbidden)
	}
	now := time.Now().UTC()
	record.Checks = make([]domain.CheckItem, 0, len(in.Checks))
	for _, name := range in.Checks {
		record.Checks = append(record.Checks, domain.CheckItem{Name: name, State: domain.CheckPending, UpdatedAt: now})
	}
	record.WorkflowFiles = in.WorkflowFiles
	record.ContextFiles = in.ContextFiles
	record.UpdatedAt = now
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		return nil, err
	}
	src, err := s.sources.Get(ctx, record.SourceID)
	if err == nil {
		for _, item := range record.Checks {
			s.reportToForge(ctx, src, &domain.CheckRun{
				Name: item.Name, HeadSHA: record.SHA, State: domain.CheckPending,
				Title: "Dagger check pending", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
			})
		}
	}
	if s.workflows != nil {
		if err := s.workflows.OnCommitLoaded(ctx, record); err != nil {
			s.logger.WithError(err).Warn("workflow catalog sync failed")
		}
	}
	return record, nil
}

// RecordComplete stores terminal check states and starts after_checks workflows.
func (s *ChecksService) RecordComplete(ctx context.Context, callerOrgID, runID string, in CompleteReportInput) (*domain.CheckRunRecord, error) {
	record, err := s.checkRuns.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if record.OrgID != callerOrgID {
		return nil, fmt.Errorf("%w: check run belongs to another org", domain.ErrForbidden)
	}
	now := time.Now().UTC()
	record.Checks = in.Checks
	record.Error = in.Error
	record.Status = "success"
	for _, item := range in.Checks {
		if item.State != domain.CheckSuccess {
			record.Status = "failed"
			break
		}
	}
	record.UpdatedAt = now
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		return nil, err
	}
	src, err := s.sources.Get(ctx, record.SourceID)
	if err == nil {
		for _, item := range record.Checks {
			s.reportToForge(ctx, src, &domain.CheckRun{
				Name: item.Name, HeadSHA: record.SHA, State: item.State,
				Title: item.Summary, DetailsURL: pipelineURL(s.publicURL, item.TraceID),
			})
		}
		if len(record.Checks) == 0 {
			state := domain.CheckSuccess
			if record.Status != "success" {
				state = domain.CheckFailure
			}
			s.reportToForge(ctx, src, &domain.CheckRun{
				Name: "dagger/checks", HeadSHA: record.SHA, State: state,
				Title: "Dagger checks completed", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
			})
		}
	}
	if record.Status == "success" && s.workflows != nil {
		if err := s.workflows.OnChecksSucceeded(ctx, record); err != nil {
			s.logger.WithError(err).Warn("after_checks workflow start failed")
		}
	}
	return record, nil
}

// ApproveForkRun releases a fork-PR hold and launches the Job.
func (s *ChecksService) ApproveForkRun(ctx context.Context, runID, approverUserID string, includeSecrets, callerIsOrgAdmin bool) (*domain.CheckRunRecord, error) {
	record, err := s.checkRuns.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if record.Status != "waiting_approval" {
		return nil, fmt.Errorf("%w: check run is not waiting for approval", domain.ErrValidation)
	}
	if includeSecrets && !callerIsOrgAdmin {
		return nil, fmt.Errorf("%w: only org admins may expose secrets to a fork PR", domain.ErrForbidden)
	}
	now := time.Now().UTC()
	record.Status = "running"
	record.ApprovedBy = approverUserID
	record.ApprovedAt = &now
	record.IncludeSecrets = includeSecrets
	record.UpdatedAt = now
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		return nil, err
	}
	src, err := s.sources.Get(ctx, record.SourceID)
	if err != nil {
		return nil, err
	}
	if err := s.dispatchCheckJob(ctx, src, record); err != nil {
		return nil, err
	}
	return record, nil
}

// Rerun resets a run and re-dispatches it.
func (s *ChecksService) Rerun(ctx context.Context, runID string) (*domain.CheckRunRecord, error) {
	record, err := s.checkRuns.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if record.IsFork && record.ApprovedAt == nil {
		return nil, fmt.Errorf("%w: fork PR run requires approval", domain.ErrForbidden)
	}
	record.Status = "running"
	record.Error = ""
	record.UpdatedAt = time.Now().UTC()
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		return nil, err
	}
	src, err := s.sources.Get(ctx, record.SourceID)
	if err != nil {
		return nil, err
	}
	if err := s.dispatchCheckJob(ctx, src, record); err != nil {
		return nil, err
	}
	return record, nil
}

// StartReconciler marks crashed/timed-out Jobs failed on the leader.
func (s *ChecksService) StartReconciler(ctx context.Context, isLeader func() bool) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if isLeader != nil && !isLeader() {
				continue
			}
			s.reconcile(ctx)
		}
	}
}

func (s *ChecksService) reconcile(ctx context.Context) {
	if s.runner == nil || s.checkRuns == nil {
		return
	}
	records, err := s.checkRuns.List(ctx, &domain.CheckRunFilter{Status: "running"})
	if err != nil {
		s.logger.WithError(err).Warn("check reconciler list failed")
		return
	}
	for _, record := range records {
		state, err := s.runner.JobStatus(ctx, record.ID)
		if err != nil || state != domain.JobStateFailed {
			continue
		}
		record.Status = "failed"
		record.Error = "check job failed or timed out"
		record.UpdatedAt = time.Now().UTC()
		if err := s.checkRuns.Upsert(ctx, record); err != nil {
			s.logger.WithError(err).Warn("check reconciler update failed")
			continue
		}
		if src, err := s.sources.Get(ctx, record.SourceID); err == nil {
			s.reportToForge(ctx, src, &domain.CheckRun{
				Name: "dagger/checks", HeadSHA: record.SHA, State: domain.CheckFailure,
				Title: "Dagger checks failed", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
			})
		}
	}
}

// reportToForge posts a check status, logging (never failing) outbound errors.
func (s *ChecksService) reportToForge(ctx context.Context, src *domain.ForgeSource, run *domain.CheckRun) {
	p, ok := s.providers[src.Provider]
	if !ok {
		return
	}
	token, err := s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.TokenCiphertext)
	if err != nil {
		s.logger.WithError(err).Warn("open forge token failed")
		return
	}
	if src.AppID != "" || src.InstallationID != "" {
		err = p.CreateCheckRun(ctx, src, token, run)
	} else {
		err = p.SetCommitStatus(ctx, src, token, run)
	}
	if err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{"source_id": src.ID, "check": run.Name}).Warn("forge status report failed")
	}
}

// forgeEventName extracts the provider event name from headers.
func forgeEventName(provider domain.ForgeKind, headers map[string]string) string {
	switch provider {
	case domain.ForgeGitHub:
		return headers["X-GitHub-Event"]
	case domain.ForgeGitLab:
		return headers["X-Gitlab-Event"]
	case domain.ForgeGitea:
		return headers["X-Gitea-Event"]
	default:
		return ""
	}
}

// pipelineURL builds a pipeline-view link, returning "" when the trace ID is
// empty or the base URL is invalid.
func pipelineURL(base, traceID string) string {
	if traceID == "" {
		return ""
	}
	u, err := domain.PipelineViewURL(base, traceID)
	if err != nil {
		return ""
	}
	return u
}

// List returns check runs matching the filter.
func (s *ChecksService) List(ctx context.Context, f *domain.CheckRunFilter) ([]*domain.CheckRunRecord, error) {
	return s.checkRuns.List(ctx, f)
}

// Get returns one check run.
func (s *ChecksService) Get(ctx context.Context, id string) (*domain.CheckRunRecord, error) {
	return s.checkRuns.Get(ctx, id)
}
