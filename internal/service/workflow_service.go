package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkflowServiceConfig bounds workflow execution.
type WorkflowServiceConfig struct {
	DefaultTimeout time.Duration
	MaxParallel    int
}

// StartWorkflowInput describes one workflow run to start.
type StartWorkflowInput struct {
	Workflow        *domain.Workflow
	OrgID           string
	ProjectID       string
	Repo            string
	SHA             string
	Ref             string
	DefaultBranch   string
	TargetBranch    string
	Tag             string
	PRNumber        int
	IsFork          bool
	IsRelease       bool
	Inputs          map[string]any
	TriggerArtifact *domain.WorkflowArtifactRef
	ContextFiles    map[string][]byte
	TriggeredBy     string
}

// WorkflowService discovers and orchestrates Dagger-native workflows.
type WorkflowService struct {
	repo      domain.WorkflowRepository
	envs      domain.EnvironmentRepository
	orgs      *OrgService
	projects  *ProjectService
	sources   domain.ForgeSourceRepository
	parser    domain.WorkflowFileParser
	evaluator domain.WorkflowExprEvaluator
	artifacts domain.ArtifactStore
	runner    domain.WorkflowJobRunner
	secrets   *SecretService
	cfg       WorkflowServiceConfig
	enabled   atomic.Bool
	logger    *logrus.Logger

	mu        sync.Mutex
	tokenMu   sync.Mutex
	runs      map[string]*domain.WorkflowRun
	runTokens map[string]string
	lastCron  map[string]string
}

// NewWorkflowService returns a WorkflowService.
func NewWorkflowService(
	repo domain.WorkflowRepository,
	envs domain.EnvironmentRepository,
	orgs *OrgService,
	projects *ProjectService,
	sources domain.ForgeSourceRepository,
	parser domain.WorkflowFileParser,
	evaluator domain.WorkflowExprEvaluator,
	artifacts domain.ArtifactStore,
	runner domain.WorkflowJobRunner,
	secrets *SecretService,
	cfg WorkflowServiceConfig,
	logger *logrus.Logger,
) *WorkflowService {
	svc := &WorkflowService{
		repo: repo, envs: envs, orgs: orgs, projects: projects, sources: sources,
		parser: parser, evaluator: evaluator, artifacts: artifacts, runner: runner,
		secrets: secrets, cfg: cfg, logger: logger,
		runs: make(map[string]*domain.WorkflowRun), runTokens: make(map[string]string),
		lastCron: make(map[string]string),
	}
	svc.enabled.Store(true)
	return svc
}

// WithEnabled gates workflow run creation. Reads always work.
func (s *WorkflowService) WithEnabled(enabled bool) *WorkflowService {
	s.enabled.Store(enabled)
	return s
}

// Enabled reports whether workflow execution is enabled.
func (s *WorkflowService) Enabled() bool { return s.enabled.Load() }

// Start validates inputs, creates a run atomically, and advances its DAG.
//
//nolint:gocritic // StartWorkflowInput is passed by value to keep the public API stable
func (s *WorkflowService) Start(ctx context.Context, in StartWorkflowInput) (*domain.WorkflowRun, error) {
	if !s.enabled.Load() {
		return nil, fmt.Errorf("%w: workflows are disabled (workflows.enabled is false)", domain.ErrValidation)
	}
	if in.Workflow == nil {
		return nil, fmt.Errorf("%w: workflow is required", domain.ErrValidation)
	}
	inputs, err := validateWorkflowInputs(in.Workflow, in.Inputs)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	git := buildGitContext(in)
	runToken, runTokenHash := newRunToken()
	run := &domain.WorkflowRun{
		Workflow:        in.Workflow.Name,
		OrgID:           in.OrgID,
		ProjectID:       in.ProjectID,
		SHA:             in.SHA,
		Ref:             in.Ref,
		Status:          "running",
		Jobs:            make(map[string]domain.WorkflowJobRun, len(in.Workflow.Jobs)),
		Spec:            in.Workflow,
		Git:             git,
		Inputs:          inputs,
		TriggerArtifact: in.TriggerArtifact,
		ContextFiles:    in.ContextFiles,
		TriggeredBy:     in.TriggeredBy,
		IsFork:          in.IsFork,
		RunTokenHash:    runTokenHash,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	for name := range in.Workflow.Jobs {
		run.Jobs[name] = domain.WorkflowJobRun{Name: name, Status: "pending"}
	}
	if s.repo != nil {
		created, err := s.repo.CreateRun(ctx, run)
		if err != nil {
			return nil, err
		}
		run = created
	} else {
		s.mu.Lock()
		run.ID = fmt.Sprintf("%s-%s-%d", in.ProjectID, workflowSlug(in.Workflow.Name), 1)
		run.Number = 1
		s.runs[run.ID] = run
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.runTokens[run.ID] = runToken
	s.mu.Unlock()
	if err := s.advanceDAG(ctx, run, ""); err != nil {
		return run, err
	}
	return run, nil
}

// advanceDAG executes every eligible job in topological order.
func (s *WorkflowService) advanceDAG(ctx context.Context, run *domain.WorkflowRun, repoRoot string) error {
	if run.Spec == nil {
		return nil
	}
	evalRoot := repoRoot
	cleanup := func() {}
	if evalRoot == "" && len(run.ContextFiles) > 0 {
		dir, err := os.MkdirTemp("", "wf-ctx-*")
		if err != nil {
			return fmt.Errorf("create workflow context dir: %w", err)
		}
		for name, data := range run.ContextFiles {
			path := filepath.Join(dir, filepath.Clean(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("materialize workflow context: %w", err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("materialize workflow context: %w", err)
			}
		}
		evalRoot = dir
		cleanup = func() { _ = os.RemoveAll(dir) }
	}
	defer cleanup()

	for _, name := range orderJobs(run.Spec.Jobs) {
		job := run.Spec.Jobs[name]
		jr := run.Jobs[name]
		if jr.Status != "pending" {
			continue
		}
		depStatus := dependencyStatus(run, job.Needs)
		switch depStatus {
		case "blocked":
			s.setJobStatus(run, name, "skipped", "")
			continue
		case "waiting":
			continue
		}
		if job.If != "" {
			ok, err := s.evalBool(job.If, buildExprEnv(run), evalRoot)
			if err != nil || !ok {
				s.setJobStatus(run, name, "skipped", "")
				continue
			}
		}
		if job.Environment != "" && jr.ApprovedAt == nil {
			now := time.Now().UTC()
			jr.Status = "waiting_approval"
			jr.Environment = job.Environment
			jr.WaitingSince = &now
			jr.ApprovalDeadline = s.approvalDeadline(ctx, run.OrgID, &job, now)
			s.updateJob(run, name, &jr)
			continue
		}
		s.executeJob(ctx, run, name, &job, jr.ApprovedAt != nil, evalRoot)
	}
	s.finishRun(run)
	return s.persist(ctx, run)
}

func (s *WorkflowService) approvalDeadline(ctx context.Context, orgID string, job *domain.WorkflowJob, now time.Time) *time.Time {
	timeout := job.ApprovalTimeout
	if timeout <= 0 && s.envs != nil && job.Environment != "" {
		if env, err := s.envs.Get(ctx, orgID, job.Environment); err == nil {
			timeout = env.ApprovalTimeout
		}
	}
	if timeout <= 0 {
		return nil
	}
	deadline := now.Add(timeout)
	return &deadline
}

// dependencyStatus reports whether a job's dependencies are satisfied.
func dependencyStatus(run *domain.WorkflowRun, needs []string) string {
	for _, dep := range needs {
		jr, ok := run.Jobs[dep]
		if !ok {
			continue
		}
		switch jr.Status {
		case "success":
		case "failed", "rejected", "skipped":
			return "blocked"
		default:
			return "waiting"
		}
	}
	return "ready"
}

// executeJob resolves secrets, interpolates templates, and runs one job.
//
//nolint:gocyclo // job execution has distinct trigger/call/secret branches
func (s *WorkflowService) executeJob(ctx context.Context, run *domain.WorkflowRun, name string, job *domain.WorkflowJob, approved bool, evalRoot string) {
	if job.Trigger != nil {
		s.executeTriggerJob(ctx, run, name, job, evalRoot)
		return
	}
	if job.Call == "" {
		s.setJobStatus(run, name, "skipped", "")
		return
	}
	if s.runner == nil {
		s.setJobStatus(run, name, "success", "")
		return
	}
	approvedEnv := ""
	if approved {
		approvedEnv = job.Environment
	}
	env := buildExprEnv(run)
	resolvedSecrets := map[string]string{}
	if len(job.Secrets) > 0 {
		if run.IsFork {
			s.setJobStatus(run, name, "failed", "secrets are not available to fork PR runs")
			return
		}
		if s.secrets != nil {
			var err error
			resolvedSecrets, err = s.secrets.ResolveJobSecrets(ctx, run.OrgID, run.ProjectID, approvedEnv, job.Secrets)
			if err != nil {
				s.setJobStatus(run, name, "failed", err.Error())
				return
			}
		}
	}
	call, err := InterpolateString(job.Call, env, s.evaluator, evalRoot)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	outputs := make(map[string]string, len(job.Outputs))
	for k, tmpl := range job.Outputs {
		v, err := InterpolateString(tmpl, env, s.evaluator, evalRoot)
		if err != nil {
			s.setJobStatus(run, name, "failed", err.Error())
			return
		}
		outputs[k] = v
	}
	token := ""
	if s.orgs != nil {
		token, _ = s.orgs.EnsureToken(ctx, run.OrgID)
	}
	cloneToken := ""
	cloneURL := ""
	if s.sources != nil && s.secrets != nil {
		if srcs, err := s.sources.ListByOrg(ctx, run.OrgID); err == nil {
			for _, src := range srcs {
				if src.Repo == run.Git.Repo {
					cloneToken, _ = s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.TokenCiphertext)
					cloneURL = domain.ForgeCloneURL(src.Provider, src.BaseURL, run.Git.Repo)
					break
				}
			}
		}
	}
	runToken, err := s.ensureRunToken(ctx, run)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	spec := &domain.WorkflowCallSpec{
		RunID: run.ID, OrgID: run.OrgID, ProjectID: run.ProjectID, JobName: name,
		Call: call, Environment: job.Environment, Timeout: job.Timeout,
		Secrets: resolvedSecrets, Repo: run.Git.Repo, SHA: run.Git.SHA, Ref: run.Git.Ref,
		Token: token, RunToken: runToken, CloneToken: cloneToken, CloneURL: cloneURL, Artifacts: job.Artifacts, UsesArtifacts: job.UsesArtifacts,
	}
	captured, err := s.runner.RunCall(ctx, spec)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	s.mu.Lock()
	jr := run.Jobs[name]
	// The runner dispatches the job asynchronously (K8sWorkflowRunner returns
	// immediately after creating the Job); completion is reported back via
	// CompleteJob, which resumes the DAG. Marking "running" here keeps
	// downstream jobs blocked until the runner reports back, so
	// needs.<job>.outputs.* is populated from the real result rather than an
	// empty map.
	jr.Status = "running"
	if len(captured) > 0 {
		jr.Outputs = captured
	}
	if len(outputs) > 0 {
		if jr.Outputs == nil {
			jr.Outputs = make(map[string]string, len(outputs))
		}
		for k, v := range outputs {
			jr.Outputs[k] = v
		}
	}
	run.Jobs[name] = jr
	run.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// executeTriggerJob starts a cross-repo workflow natively on the leader.
//
//nolint:gocyclo // cross-repo trigger resolution and wait loop
func (s *WorkflowService) executeTriggerJob(ctx context.Context, run *domain.WorkflowRun, name string, job *domain.WorkflowJob, evalRoot string) {
	if s.projects == nil || s.repo == nil {
		s.setJobStatus(run, name, "failed", "cross-repo trigger unavailable")
		return
	}
	targetProj, err := s.projects.GetOrCreateByName(ctx, run.OrgID, job.Trigger.Repo)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	if targetProj.OrgID != "" && targetProj.OrgID != run.OrgID {
		s.setJobStatus(run, name, "failed", "cross-org trigger forbidden")
		return
	}
	def, err := s.repo.GetDef(ctx, run.OrgID, targetProj.ID, job.Trigger.Workflow)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	if !def.Workflow.On.WorkflowCall && def.Workflow.On.WorkflowDispatch == nil {
		s.setJobStatus(run, name, "failed", "target workflow is not callable")
		return
	}
	env := buildExprEnv(run)
	inputs := make(map[string]any, len(job.Trigger.Inputs))
	for k, v := range job.Trigger.Inputs {
		if str, ok := v.(string); ok {
			out, err := InterpolateString(str, env, s.evaluator, evalRoot)
			if err != nil {
				s.setJobStatus(run, name, "failed", err.Error())
				return
			}
			inputs[k] = out
			continue
		}
		inputs[k] = v
	}
	child, err := s.Start(ctx, StartWorkflowInput{
		Workflow: &def.Workflow, OrgID: run.OrgID, ProjectID: targetProj.ID, Repo: def.Repo,
		SHA: run.Git.SHA, Ref: run.Git.Ref, DefaultBranch: run.Git.DefaultBranch,
		TargetBranch: run.Git.TargetBranch, Tag: run.Git.Tag, PRNumber: run.Git.PRNumber,
		Inputs: inputs, ContextFiles: def.ContextFiles, TriggeredBy: run.TriggeredBy,
	})
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	s.mu.Lock()
	jr := run.Jobs[name]
	jr.TriggeredRunID = child.ID
	run.Jobs[name] = jr
	s.mu.Unlock()
	if !job.Trigger.Wait {
		s.setJobStatus(run, name, "success", "")
		return
	}
	timeout := job.Trigger.Timeout
	if timeout <= 0 {
		timeout = s.cfg.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.setJobStatus(run, name, "failed", "trigger wait timed out")
			return
		}
		current, err := s.repo.GetRun(ctx, child.ID)
		if err == nil {
			switch current.Status {
			case "success":
				s.setJobStatus(run, name, "success", "")
				return
			case "failed":
				s.setJobStatus(run, name, "failed", "remote workflow failed")
				return
			}
		}
		select {
		case <-ctx.Done():
			s.setJobStatus(run, name, "failed", ctx.Err().Error())
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Approve releases an environment gate and resumes the DAG.
func (s *WorkflowService) Approve(ctx context.Context, runID, jobName string, caller *domain.Identity) (*domain.WorkflowRun, error) {
	run, err := s.loadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	jr, ok := run.Jobs[jobName]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if jr.Status != "waiting_approval" {
		return nil, fmt.Errorf("%w: job is not waiting for approval", domain.ErrValidation)
	}
	job := run.Spec.Jobs[jobName]
	envCfg, _ := s.envCfg(ctx, run.OrgID, job.Environment)
	if err := s.authorizeApprover(ctx, run, caller, envCfg, job.Environment); err != nil {
		return nil, err
	}
	if envCfg != nil && envCfg.PreventSelfApproval && run.TriggeredBy != "" && run.TriggeredBy == caller.UserID {
		return nil, fmt.Errorf("%w: self-approval is forbidden for environment %s", domain.ErrForbidden, job.Environment)
	}
	now := time.Now().UTC()
	if jr.ApprovalDeadline != nil && now.After(*jr.ApprovalDeadline) {
		jr.Status = "failed"
		jr.Error = "approval timed out"
		s.updateJob(run, jobName, &jr)
		s.skipDependents(run, jobName)
		s.finishRun(run)
		_ = s.persist(ctx, run)
		return run, fmt.Errorf("%w: approval deadline expired", domain.ErrValidation)
	}
	jr.Status = "running"
	jr.ApprovedBy = caller.UserID
	jr.ApprovedAt = &now
	s.updateJob(run, jobName, &jr)
	if err := s.persist(ctx, run); err != nil {
		return nil, err
	}
	s.executeJob(ctx, run, jobName, &job, true, "")
	if err := s.advanceDAG(ctx, run, ""); err != nil {
		return run, err
	}
	return run, nil
}

// Reject fails an environment gate and skips downstream jobs.
func (s *WorkflowService) Reject(ctx context.Context, runID, jobName string, caller *domain.Identity, reason string) (*domain.WorkflowRun, error) {
	run, err := s.loadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	jr, ok := run.Jobs[jobName]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if jr.Status != "waiting_approval" {
		return nil, fmt.Errorf("%w: job is not waiting for approval", domain.ErrValidation)
	}
	job := run.Spec.Jobs[jobName]
	envCfg, _ := s.envCfg(ctx, run.OrgID, job.Environment)
	if err := s.authorizeApprover(ctx, run, caller, envCfg, job.Environment); err != nil {
		return nil, err
	}
	jr.Status = "failed"
	jr.RejectedBy = caller.UserID
	jr.Error = reason
	s.updateJob(run, jobName, &jr)
	s.skipDependents(run, jobName)
	s.finishRun(run)
	if err := s.persist(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *WorkflowService) authorizeApprover(ctx context.Context, run *domain.WorkflowRun, caller *domain.Identity, envCfg *domain.Environment, envName string) error {
	if caller == nil {
		return fmt.Errorf("%w: unauthenticated", domain.ErrForbidden)
	}
	if caller.Role == domain.RoleAdmin {
		return nil
	}
	if s.orgs != nil && s.orgs.IsOrgAdmin(ctx, run.OrgID, caller.UserID) {
		return nil
	}
	if envCfg != nil {
		for _, gid := range envCfg.ApproverGroupIDs {
			if caller.HasGroup(gid) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: caller is not an authorized approver for environment %s", domain.ErrForbidden, envName)
}

func (s *WorkflowService) envCfg(ctx context.Context, orgID, name string) (*domain.Environment, error) {
	if s.envs == nil || name == "" {
		return nil, domain.ErrNotFound
	}
	return s.envs.Get(ctx, orgID, name)
}

// OnCommitLoaded syncs the catalog and starts pre-check workflows.
func (s *WorkflowService) OnCommitLoaded(ctx context.Context, record *domain.CheckRunRecord) error {
	if !s.enabled.Load() {
		return nil
	}
	defs, workflows, err := s.parseWorkflowFiles(record)
	if err != nil {
		return err
	}
	if record.DefaultBranch != "" && record.Ref == fmt.Sprintf("refs/heads/%s", record.DefaultBranch) && s.repo != nil {
		if err := s.repo.SyncProjectCatalog(ctx, record.OrgID, record.ProjectID, defs); err != nil {
			return err
		}
	}
	for _, wf := range workflows {
		if wf.AfterChecks {
			continue
		}
		if !workflowMatchesEvent(wf, record.Event) {
			continue
		}
		if _, err := s.Start(ctx, StartWorkflowInput{
			Workflow: wf, OrgID: record.OrgID, ProjectID: record.ProjectID, Repo: record.Repo,
			SHA: record.SHA, Ref: record.Ref, DefaultBranch: record.DefaultBranch,
			TargetBranch: record.TargetBranch, Tag: record.Tag, PRNumber: record.PRNumber,
			IsFork: record.IsFork, IsRelease: record.IsRelease, ContextFiles: record.ContextFiles, TriggeredBy: record.ApprovedBy,
		}); err != nil {
			s.logger.WithError(err).WithField("workflow", wf.Name).Warn("workflow start failed")
		}
	}
	return nil
}

// OnChecksSucceeded starts after_checks workflows.
func (s *WorkflowService) OnChecksSucceeded(ctx context.Context, record *domain.CheckRunRecord) error {
	if !s.enabled.Load() {
		return nil
	}
	_, workflows, err := s.parseWorkflowFiles(record)
	if err != nil {
		return err
	}
	for _, wf := range workflows {
		if !wf.AfterChecks || !workflowMatchesEvent(wf, record.Event) {
			continue
		}
		if _, err := s.Start(ctx, StartWorkflowInput{
			Workflow: wf, OrgID: record.OrgID, ProjectID: record.ProjectID, Repo: record.Repo,
			SHA: record.SHA, Ref: record.Ref, DefaultBranch: record.DefaultBranch,
			TargetBranch: record.TargetBranch, Tag: record.Tag, PRNumber: record.PRNumber,
			IsFork: record.IsFork, IsRelease: record.IsRelease, ContextFiles: record.ContextFiles, TriggeredBy: record.ApprovedBy,
		}); err != nil {
			s.logger.WithError(err).WithField("workflow", wf.Name).Warn("workflow start failed")
		}
	}
	return nil
}

func (s *WorkflowService) parseWorkflowFiles(record *domain.CheckRunRecord) ([]*domain.WorkflowDef, []*domain.Workflow, error) {
	defs := make([]*domain.WorkflowDef, 0, len(record.WorkflowFiles))
	workflows := make([]*domain.Workflow, 0, len(record.WorkflowFiles))
	for name, raw := range record.WorkflowFiles {
		wf, err := s.parser.Parse(name, []byte(raw))
		if err != nil {
			return nil, nil, err
		}
		workflows = append(workflows, wf)
		defs = append(defs, &domain.WorkflowDef{
			ID:    fmt.Sprintf("%s/%s/%s", record.OrgID, record.ProjectID, wf.Name),
			OrgID: record.OrgID, ProjectID: record.ProjectID, Repo: record.Repo,
			DefaultRef: record.Ref, DefaultSHA: record.SHA, Workflow: *wf,
			ContextFiles: record.ContextFiles, UpdatedAt: time.Now().UTC(),
		})
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].Name < workflows[j].Name })
	return defs, workflows, nil
}

func workflowMatchesEvent(wf *domain.Workflow, event string) bool {
	switch event {
	case "push":
		return wf.On.Push
	case "pull_request":
		return wf.On.PullRequest
	case "tag":
		return wf.On.Tag
	case "release":
		return wf.On.Release
	default:
		return false
	}
}

// StartScheduler runs cron schedules and sweeps expired approvals on the leader.
func (s *WorkflowService) StartScheduler(ctx context.Context, isLeader func() bool) {
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
			s.sweepApprovals(ctx)
			s.fireSchedules(ctx)
		}
	}
}

func (s *WorkflowService) sweepApprovals(ctx context.Context) {
	if s.repo == nil {
		return
	}
	runs, err := s.repo.ListRuns(ctx, &domain.WorkflowRunFilter{Status: "waiting_approval"})
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for i := range runs {
		run := runs[i]
		var timedOut []string
		for name, jr := range run.Jobs { //nolint:gocritic // map values are not addressable
			if jr.Status == "waiting_approval" && jr.ApprovalDeadline != nil && now.After(*jr.ApprovalDeadline) {
				jr.Status = "failed"
				jr.Error = "approval timed out"
				run.Jobs[name] = jr
				timedOut = append(timedOut, name)
			}
		}
		if len(timedOut) == 0 {
			continue
		}
		for _, name := range timedOut {
			s.skipDependents(run, name)
		}
		s.finishRun(run)
		_ = s.persist(ctx, run)
	}
}

func (s *WorkflowService) fireSchedules(ctx context.Context) {
	if s.repo == nil {
		return
	}
	defs, err := s.repo.ListDefs(ctx, "", "")
	if err != nil {
		return
	}
	now := time.Now().UTC()
	minute := now.Format("2006-01-02T15:04")
	for _, def := range defs {
		for _, spec := range def.Workflow.On.Schedule {
			schedule, err := cron.ParseStandard(spec)
			if err != nil {
				continue
			}
			prev := now.Add(-time.Minute)
			next := schedule.Next(prev)
			if next.After(now) {
				continue
			}
			key := fmt.Sprintf("%s/%s", def.ID, spec)
			s.mu.Lock()
			if s.lastCron[key] == minute {
				s.mu.Unlock()
				continue
			}
			s.lastCron[key] = minute
			s.mu.Unlock()
			wf := def.Workflow
			if _, err := s.Start(ctx, StartWorkflowInput{
				Workflow: &wf, OrgID: def.OrgID, ProjectID: def.ProjectID, Repo: def.Repo,
				SHA: def.DefaultSHA, Ref: def.DefaultRef, DefaultBranch: def.DefaultRef,
				ContextFiles: def.ContextFiles,
			}); err != nil {
				s.logger.WithError(err).WithField("workflow", def.Workflow.Name).Warn("scheduled workflow start failed")
			}
		}
	}
}

func (s *WorkflowService) loadRun(ctx context.Context, runID string) (*domain.WorkflowRun, error) {
	if s.repo != nil {
		return s.repo.GetRun(ctx, runID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return run, nil
}

func (s *WorkflowService) persist(ctx context.Context, run *domain.WorkflowRun) error {
	if s.repo != nil {
		return s.repo.UpsertRun(ctx, run)
	}
	s.mu.Lock()
	s.runs[run.ID] = run
	s.mu.Unlock()
	return nil
}

// ensureRunToken returns the plaintext callback token for run, generating and
// persisting a new hash when the in-memory token is unavailable (e.g. after a
// leader restart). Only the hash is replicated; the plaintext lives in memory
// for the lifetime of the process.
//
// When a re-mint is required, the previous hash is retained in
// PreviousRunTokenHash so in-flight Jobs dispatched before the restart can
// still report completion. Minting is serialized with tokenMu to prevent two
// concurrent dispatches from minting different tokens for the same run.
func (s *WorkflowService) ensureRunToken(ctx context.Context, run *domain.WorkflowRun) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if token := s.runTokens[run.ID]; token != "" {
		return token, nil
	}
	token, hash := newRunToken()
	run.PreviousRunTokenHash = run.RunTokenHash
	run.RunTokenHash = hash
	if err := s.persist(ctx, run); err != nil {
		return "", fmt.Errorf("persist workflow run token: %w", err)
	}
	s.runTokens[run.ID] = token
	return token, nil
}

// ListDefs lists the indexed workflow catalog.
func (s *WorkflowService) ListDefs(ctx context.Context, orgID, projectID string) ([]*domain.WorkflowDef, error) {
	if s.repo == nil {
		return nil, nil
	}
	return s.repo.ListDefs(ctx, orgID, projectID)
}

// ListRuns lists workflow runs.
func (s *WorkflowService) ListRuns(ctx context.Context, f *domain.WorkflowRunFilter) ([]*domain.WorkflowRun, error) {
	if s.repo == nil {
		return nil, nil
	}
	return s.repo.ListRuns(ctx, f)
}

// GetRun returns one workflow run.
func (s *WorkflowService) GetRun(ctx context.Context, runID string) (*domain.WorkflowRun, error) {
	return s.loadRun(ctx, runID)
}

// Dispatch starts a workflow_dispatch run from the indexed catalog.
func (s *WorkflowService) Dispatch(ctx context.Context, orgID, projectID, workflowName, ref string, inputs map[string]any, caller *domain.Identity) (*domain.WorkflowRun, error) {
	if !s.enabled.Load() {
		return nil, fmt.Errorf("%w: workflows are disabled (workflows.enabled is false)", domain.ErrValidation)
	}
	if s.repo == nil {
		return nil, domain.ErrNotFound
	}
	def, err := s.repo.GetDef(ctx, orgID, projectID, workflowName)
	if err != nil {
		return nil, err
	}
	if def.Workflow.On.WorkflowDispatch == nil {
		return nil, fmt.Errorf("%w: workflow does not accept workflow_dispatch", domain.ErrValidation)
	}
	triggeredBy := ""
	if caller != nil {
		triggeredBy = caller.UserID
	}
	return s.Start(ctx, StartWorkflowInput{
		Workflow: &def.Workflow, OrgID: orgID, ProjectID: projectID, Repo: def.Repo,
		SHA: def.DefaultSHA, Ref: ref, DefaultBranch: def.DefaultRef,
		Inputs: inputs, ContextFiles: def.ContextFiles, TriggeredBy: triggeredBy,
	})
}

func (s *WorkflowService) evalBool(expression string, env map[string]any, repoRoot string) (bool, error) {
	if s.evaluator == nil {
		return true, nil
	}
	out, err := s.evaluator.Eval(expression, env, repoRoot)
	if err != nil {
		return false, err
	}
	b, ok := out.(bool)
	return ok && b, nil
}

func (s *WorkflowService) setJobStatus(run *domain.WorkflowRun, name, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jr := run.Jobs[name]
	jr.Status = status
	jr.Error = errMsg
	run.Jobs[name] = jr
	run.UpdatedAt = time.Now().UTC()
}

func (s *WorkflowService) updateJob(run *domain.WorkflowRun, name string, jr *domain.WorkflowJobRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.Jobs[name] = *jr
	run.UpdatedAt = time.Now().UTC()
}

// skipDependents marks every transitive dependent of jobName as skipped.
func (s *WorkflowService) skipDependents(run *domain.WorkflowRun, jobName string) {
	if run.Spec == nil {
		return
	}
	skipped := map[string]bool{jobName: true}
	for changed := true; changed; {
		changed = false
		for name, job := range run.Spec.Jobs { //nolint:gocritic // map values are not addressable
			if skipped[name] {
				continue
			}
			for _, dep := range job.Needs {
				if skipped[dep] {
					skipped[name] = true
					s.setJobStatus(run, name, "skipped", "")
					changed = true
					break
				}
			}
		}
	}
}

func (s *WorkflowService) finishRun(run *domain.WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "success"
	for _, jr := range run.Jobs { //nolint:gocritic // map values are not addressable
		switch jr.Status {
		case "failed", "rejected":
			status = "failed"
		case "waiting_approval":
			if status != "failed" {
				status = "waiting_approval"
			}
		case "pending", "running":
			if status == "success" {
				status = "running"
			}
		}
	}
	run.Status = status
	run.UpdatedAt = time.Now().UTC()
}

// buildGitContext derives the git context from a start input.
//
//nolint:gocritic // mirrors the StartWorkflowInput public API
func buildGitContext(in StartWorkflowInput) domain.WorkflowGitContext {
	branch := strings.TrimPrefix(in.Ref, "refs/heads/")
	isPR := strings.HasPrefix(in.Ref, "refs/pull/") || in.PRNumber > 0
	isTag := strings.HasPrefix(in.Ref, "refs/tags/") || in.Tag != ""
	tag := in.Tag
	if tag == "" && isTag {
		tag = strings.TrimPrefix(in.Ref, "refs/tags/")
	}
	tagVersion := ""
	if tag != "" {
		tagVersion = strings.TrimPrefix(tag, "v")
	}
	return domain.WorkflowGitContext{
		SHA: in.SHA, ShortSHA: shortSHA(in.SHA), Ref: in.Ref, Branch: branch,
		TargetBranch: in.TargetBranch, DefaultBranch: in.DefaultBranch, Tag: tag,
		TagVersion: tagVersion, Repo: in.Repo, PRNumber: in.PRNumber,
		IsBranch: strings.HasPrefix(in.Ref, "refs/heads/"), IsDefaultBranch: in.DefaultBranch != "" && branch == in.DefaultBranch,
		IsPR: isPR, IsTag: isTag, IsRelease: in.IsRelease || isTag,
	}
}

// buildExprEnv builds the full ${{ ... }} evaluation context for a run.
func buildExprEnv(run *domain.WorkflowRun) map[string]any {
	git := map[string]any{
		"sha": run.Git.SHA, "short_sha": run.Git.ShortSHA, "ref": run.Git.Ref,
		"branch": run.Git.Branch, "target_branch": run.Git.TargetBranch,
		"default_branch": run.Git.DefaultBranch, "tag": run.Git.Tag,
		"tag_version": run.Git.TagVersion, "repo": run.Git.Repo, "pr_number": run.Git.PRNumber,
		"is_branch": run.Git.IsBranch, "is_default_branch": run.Git.IsDefaultBranch,
		"is_pr": run.Git.IsPR, "is_tag": run.Git.IsTag, "is_release": run.Git.IsRelease,
	}
	inputs := map[string]any{}
	if run.Spec != nil {
		for k, in := range run.Spec.Inputs {
			inputs[k] = in.Default
		}
	}
	for k, v := range run.Inputs {
		inputs[k] = v
	}
	needs := map[string]any{}
	for name, jr := range run.Jobs { //nolint:gocritic // map values are not addressable
		needs[name] = map[string]any{"status": jr.Status, "outputs": nonNilMap(jr.Outputs)}
	}
	artifact := map[string]any{}
	if run.TriggerArtifact != nil {
		artifact = map[string]any{
			"name": run.TriggerArtifact.Name, "key": run.TriggerArtifact.Key,
			"sha256": run.TriggerArtifact.SHA256, "run_id": run.TriggerArtifact.RunID,
			"project_id": run.TriggerArtifact.ProjectID,
		}
	}
	return map[string]any{
		"git": git, "run": map[string]any{"id": run.ID, "number": run.Number},
		"inputs": inputs, "needs": needs, "artifact": artifact,
	}
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

var interpolationRe = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// InterpolateString evaluates every ${{ ... }} span in s.
func InterpolateString(s string, env map[string]any, eval domain.WorkflowExprEvaluator, repoRoot string) (string, error) {
	if eval == nil || !strings.Contains(s, "${{") {
		return s, nil
	}
	var firstErr error
	out := interpolationRe.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		v, err := eval.Eval(match, env, repoRoot)
		if err != nil {
			firstErr = err
			return match
		}
		return fmt.Sprintf("%v", v)
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// validateWorkflowInputs applies typed workflow_dispatch input validation.
func validateWorkflowInputs(wf *domain.Workflow, inputs map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(inputs))
	for k, v := range inputs {
		out[k] = v
	}
	declared := wf.Inputs
	if wf.On.WorkflowDispatch != nil {
		declared = wf.On.WorkflowDispatch.Inputs
	}
	for name, in := range declared {
		value, ok := out[name]
		if !ok || value == nil {
			if in.Default != nil {
				out[name] = in.Default
				continue
			}
			if in.Required {
				return nil, fmt.Errorf("%w: input %s is required", domain.ErrValidation, name)
			}
			continue
		}
		switch in.Type {
		case "boolean":
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%w: input %s must be a boolean", domain.ErrValidation, name)
			}
		case "number":
			switch value.(type) {
			case int, int32, int64, float32, float64:
			default:
				return nil, fmt.Errorf("%w: input %s must be a number", domain.ErrValidation, name)
			}
		case "choice":
			str := fmt.Sprintf("%v", value)
			valid := false
			for _, opt := range in.Options {
				if opt == str {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("%w: input %s is not a valid choice", domain.ErrValidation, name)
			}
		}
	}
	return out, nil
}

// orderJobs returns job names in dependency order (needs first), breaking ties
// alphabetically for deterministic runs.
func orderJobs(jobs map[string]domain.WorkflowJob) []string {
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	visited := make(map[string]bool, len(names))
	ordered := make([]string, 0, len(names))
	var visit func(string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range jobs[name].Needs {
			if _, ok := jobs[dep]; ok {
				visit(dep)
			}
		}
		ordered = append(ordered, name)
	}
	for _, name := range names {
		visit(name)
	}
	return ordered
}

func workflowSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// CompleteJob records a runner-reported job completion and resumes the DAG.
func (s *WorkflowService) CompleteJob(ctx context.Context, runID, jobName string, outputs map[string]string, artifacts []string, errMsg string) (*domain.WorkflowRun, error) {
	run, err := s.loadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	jr, ok := run.Jobs[jobName]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if errMsg != "" {
		jr.Status = "failed"
		jr.Error = errMsg
	} else {
		jr.Status = "success"
		if jr.Outputs == nil {
			jr.Outputs = make(map[string]string, len(outputs))
		}
		for k, v := range outputs {
			jr.Outputs[k] = v
		}
		jr.Artifacts = artifacts
	}
	s.updateJob(run, jobName, &jr)
	if err := s.advanceDAG(ctx, run, ""); err != nil {
		return run, err
	}
	return run, nil
}

// OnArtifactPublished starts workflows whose on: artifact trigger matches the
// published artifact name.
func (s *WorkflowService) OnArtifactPublished(ctx context.Context, orgID, projectID, runID, name, key string) error {
	if !s.enabled.Load() || s.repo == nil {
		return nil
	}
	defs, err := s.repo.ListDefs(ctx, orgID, projectID)
	if err != nil {
		return err
	}
	for i := range defs {
		def := defs[i]
		matched := false
		for _, trigger := range def.Workflow.On.Artifact {
			if trigger == name || trigger == "*" {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		wf := def.Workflow
		if _, err := s.Start(ctx, StartWorkflowInput{
			Workflow: &wf, OrgID: orgID, ProjectID: projectID, Repo: def.Repo,
			SHA: def.DefaultSHA, Ref: def.DefaultRef, DefaultBranch: def.DefaultRef,
			ContextFiles: def.ContextFiles,
			TriggerArtifact: &domain.WorkflowArtifactRef{
				Name: name, Key: key, RunID: runID, ProjectID: projectID,
			},
		}); err != nil {
			s.logger.WithError(err).WithField("workflow", def.Workflow.Name).Warn("artifact workflow start failed")
		}
	}
	return nil
}
