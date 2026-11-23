package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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
	sources       domain.ForgeSourceRepository
	secrets       *SecretService
	projects      *ProjectService
	orgs          *OrgService
	runner        domain.CheckJobRunner
	checkRuns     domain.CheckRunRepository
	workflows     *WorkflowService
	providers     map[domain.ForgeKind]domain.ForgeProvider
	publicURL     string
	webhookSecret string
	enabled       atomic.Bool
	logger        *logrus.Logger
}

// WebhookResult is the outcome of one forge delivery. Ignored deliveries carry
// a reason and no event; accepted deliveries carry the parsed event.
type WebhookResult struct {
	Event    *domain.WebhookEvent
	Ignored  bool
	Reason   string
	RawEvent string
}

// NewChecksService returns a ChecksService. webhookSecret is the fallback HMAC
// secret used to verify deliveries for sources that have none.
func NewChecksService(sources domain.ForgeSourceRepository, secrets *SecretService, projects *ProjectService, orgs *OrgService, runner domain.CheckJobRunner, checkRuns domain.CheckRunRepository, workflows *WorkflowService, providers []domain.ForgeProvider, publicURL, webhookSecret string, logger *logrus.Logger) *ChecksService {
	m := make(map[domain.ForgeKind]domain.ForgeProvider, len(providers))
	for _, p := range providers {
		m[p.Kind()] = p
	}
	svc := &ChecksService{
		sources: sources, secrets: secrets, projects: projects, orgs: orgs, runner: runner,
		checkRuns: checkRuns, workflows: workflows, providers: m, publicURL: publicURL,
		webhookSecret: webhookSecret, logger: logger,
	}
	svc.enabled.Store(true)
	return svc
}

// WithEnabled gates check-run creation and dispatch. Reads always work.
func (s *ChecksService) WithEnabled(enabled bool) *ChecksService {
	s.enabled.Store(enabled)
	return s
}

// Enabled reports whether Cloud Checks dispatch is enabled.
func (s *ChecksService) Enabled() bool { return s.enabled.Load() }

// HandleWebhook verifies and dispatches a forge webhook. HMAC failures return
// ErrForbidden and must never be recorded as token failures (shared NAT IPs).
// The ordering is fail-closed: load source, verify signature, honour the
// enabled gate, then classify before any project/record/dispatch/forge write.
func (s *ChecksService) HandleWebhook(ctx context.Context, provider domain.ForgeKind, sourceID string, headers map[string]string, body []byte) (*WebhookResult, error) {
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
	// A non-empty per-source secret always wins; the configured fallback only
	// covers sources stored without one. Both empty stays fail-closed.
	if secret == "" {
		secret = s.webhookSecret
	}
	if !p.VerifyWebhook(src, secret, headers, body) {
		return nil, fmt.Errorf("%w: invalid webhook signature", domain.ErrForbidden)
	}
	rawEvent := forgeEventName(provider, headers)
	if !s.enabled.Load() {
		return &WebhookResult{Ignored: true, Reason: "checks_disabled", RawEvent: rawEvent}, nil
	}
	disposition, err := domain.ClassifyAndParseForgeWebhook(provider, rawEvent, body)
	if err != nil {
		return nil, err
	}
	if !disposition.Accepted {
		return &WebhookResult{Ignored: true, Reason: disposition.Reason, RawEvent: disposition.RawEvent}, nil
	}
	ev := disposition.Event
	if ev.Repo == "" {
		ev.Repo = src.Repo
	}
	proj, err := s.projects.GetOrCreateByName(ctx, src.OrgID, ev.Repo)
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
	// A duplicate delivery for an already-active run is an idempotent no-op: it
	// must not rotate the run token or re-dispatch the Job (webhook replays).
	if s.checkRuns != nil {
		if existing, gerr := s.checkRuns.Get(ctx, record.ID); gerr == nil && existing.Status == "running" {
			return &WebhookResult{Event: ev, RawEvent: rawEvent}, nil
		}
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
		return &WebhookResult{Event: ev, RawEvent: rawEvent}, nil
	}
	if err := s.dispatchCheckJob(ctx, src, record); err != nil {
		s.failDispatch(ctx, src, record, err)
		return &WebhookResult{Event: ev, RawEvent: rawEvent}, err
	}
	return &WebhookResult{Event: ev, RawEvent: rawEvent}, nil
}

// failDispatch transitions a run to "failed" after a dispatch error so it is
// never stranded in "running", and reports the failure to the forge.
func (s *ChecksService) failDispatch(ctx context.Context, src *domain.ForgeSource, record *domain.CheckRunRecord, cause error) {
	record.Status = "failed"
	record.Error = cause.Error()
	record.UpdatedAt = time.Now().UTC()
	if s.checkRuns != nil {
		if err := s.checkRuns.Upsert(ctx, record); err != nil {
			s.logger.WithError(err).Warn("persist failed dispatch state failed")
		}
	}
	s.reportToForge(ctx, src, &domain.CheckRun{
		Name: "dagger/checks", HeadSHA: record.SHA, State: domain.CheckFailure,
		Title: "Dagger checks failed to dispatch", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
	})
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
	runToken, runTokenHash := newRunToken()
	record.RunTokenHash = runTokenHash
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		return fmt.Errorf("persist check run token: %w", err)
	}
	spec := &domain.CheckRunSpec{
		ID: record.ID, OrgID: record.OrgID, ProjectID: record.ProjectID, SourceID: record.SourceID,
		Provider: record.Provider, Repo: record.Repo, SHA: record.SHA, Ref: record.Ref,
		DefaultBranch: record.DefaultBranch, TargetBranch: record.TargetBranch, Tag: record.Tag,
		PRNumber: record.PRNumber, IsFork: record.IsFork, IsRelease: record.IsRelease,
		Event: record.Event, Token: token, RunToken: runToken, CloneToken: cloneToken, Secrets: secrets,
		CloneURL: domain.ForgeCloneURL(src.Provider, src.BaseURL, record.Repo),
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
	if !s.enabled.Load() {
		return nil, fmt.Errorf("%w: cloud checks are disabled (checks.enabled is false)", domain.ErrValidation)
	}
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
	if !s.enabled.Load() {
		return nil, fmt.Errorf("%w: cloud checks are disabled (checks.enabled is false)", domain.ErrValidation)
	}
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

// reconcileTimeout bounds how long a record may stay "running" with an unknown
// Job before the reconciler marks it failed (a Job that never appeared, or a
// lost Job after a crash).
const reconcileTimeout = 5 * time.Minute

func (s *ChecksService) reconcile(ctx context.Context) {
	if s.runner == nil || s.checkRuns == nil {
		return
	}
	records, err := s.checkRuns.List(ctx, &domain.CheckRunFilter{Status: "running"})
	if err != nil {
		s.logger.WithError(err).Warn("check reconciler list failed")
		return
	}
	now := time.Now().UTC()
	for _, record := range records {
		state, err := s.runner.JobStatus(ctx, record.ID)
		if err != nil {
			continue
		}
		switch {
		case state == domain.JobStateFailed:
			s.failReconciled(ctx, record, "check job failed or timed out")
		case state == domain.JobStateUnknown && now.Sub(record.UpdatedAt) > reconcileTimeout:
			// A record stuck "running" with no Job is a lost dispatch (or a
			// Job that never appeared); fail it so it is never stranded.
			s.failReconciled(ctx, record, "check job not found (reconciler timeout)")
		}
	}
}

// failReconciled transitions a run to failed and reports CheckFailure.
func (s *ChecksService) failReconciled(ctx context.Context, record *domain.CheckRunRecord, reason string) {
	record.Status = "failed"
	record.Error = reason
	record.UpdatedAt = time.Now().UTC()
	if err := s.checkRuns.Upsert(ctx, record); err != nil {
		s.logger.WithError(err).Warn("check reconciler update failed")
		return
	}
	if src, err := s.sources.Get(ctx, record.SourceID); err == nil {
		s.reportToForge(ctx, src, &domain.CheckRun{
			Name: "dagger/checks", HeadSHA: record.SHA, State: domain.CheckFailure,
			Title: "Dagger checks failed", DetailsURL: fmt.Sprintf("%s/checks", s.publicURL),
		})
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

// Delete removes one check run. It is idempotent: deleting an already-absent
// run is not an error.
func (s *ChecksService) Delete(ctx context.Context, id string) error {
	if err := s.checkRuns.Delete(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return nil
}
