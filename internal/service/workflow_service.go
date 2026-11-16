package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkflowService discovers and orchestrates Dagger-native workflows.
type WorkflowService struct {
	parser    domain.WorkflowFileParser
	evaluator domain.WorkflowExprEvaluator
	artifacts domain.ArtifactStore
	runner    domain.WorkflowJobRunner
	secrets   *SecretService
	logger    *logrus.Logger

	mu      sync.Mutex
	runs    map[string]*domain.WorkflowRun
	numbers map[string]int64
}

// NewWorkflowService returns a WorkflowService.
func NewWorkflowService(parser domain.WorkflowFileParser, evaluator domain.WorkflowExprEvaluator, artifacts domain.ArtifactStore, runner domain.WorkflowJobRunner, secrets *SecretService, logger *logrus.Logger) *WorkflowService {
	return &WorkflowService{parser: parser, evaluator: evaluator, artifacts: artifacts, runner: runner, secrets: secrets, logger: logger, runs: make(map[string]*domain.WorkflowRun), numbers: make(map[string]int64)}
}

// Load discovers .dagger/workflows/*.y{a,}ml under root.
func (s *WorkflowService) Load(root string) ([]*domain.Workflow, error) {
	dir := filepath.Join(root, ".dagger", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workflows dir: %w", err)
	}
	var out []*domain.Workflow
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- name is a directory entry under the repo workflow dir
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", name, err)
		}
		wf, err := s.parser.Parse(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Start creates a run and executes its jobs in dependency order.
func (s *WorkflowService) Start(ctx context.Context, wf *domain.Workflow, orgID, projectID, sha, ref string, inputs map[string]any) (*domain.WorkflowRun, error) {
	s.mu.Lock()
	s.numbers[wf.Name]++
	number := s.numbers[wf.Name]
	run := &domain.WorkflowRun{
		ID:        fmt.Sprintf("%s-%d", workflowSlug(wf.Name), number),
		Workflow:  wf.Name,
		OrgID:     orgID,
		ProjectID: projectID,
		SHA:       sha,
		Ref:       ref,
		Status:    "running",
		Number:    number,
		Jobs:      make(map[string]domain.WorkflowJobRun, len(wf.Jobs)),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	for name := range wf.Jobs {
		run.Jobs[name] = domain.WorkflowJobRun{Name: name, Status: "pending"}
	}
	s.runs[run.ID] = run
	s.mu.Unlock()

	env := map[string]any{
		"git": map[string]any{
			"sha": sha, "short_sha": shortSHA(sha), "branch": ref, "is_branch": strings.HasPrefix(ref, "refs/heads/"),
			"is_pr": strings.HasPrefix(ref, "refs/pull/"), "is_tag": strings.HasPrefix(ref, "refs/tags/"),
		},
		"run":    map[string]any{"number": number},
		"inputs": inputs,
	}
	for _, name := range orderJobs(wf.Jobs) {
		job := wf.Jobs[name]
		if job.If != "" {
			ok, err := s.evalBool(job.If, env)
			if err != nil || !ok {
				s.setJobStatus(run, name, "skipped", "")
				continue
			}
		}
		if job.Environment != "" {
			s.setJobStatus(run, name, "waiting_approval", "")
			continue
		}
		s.executeJob(ctx, run, name, &job)
	}
	s.finishRun(run)
	return run, nil
}

func (s *WorkflowService) executeJob(ctx context.Context, run *domain.WorkflowRun, name string, job *domain.WorkflowJob) {
	if job.Trigger != nil {
		// Cross-repo triggers execute natively in the leader state machine.
		s.setJobStatus(run, name, "success", "")
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
	spec := domain.WorkflowCallSpec{
		RunID: run.ID, OrgID: run.OrgID, ProjectID: run.ProjectID, JobName: name,
		Call: job.Call, Environment: job.Environment, Timeout: job.Timeout,
	}
	outputs, err := s.runner.RunCall(ctx, &spec)
	if err != nil {
		s.setJobStatus(run, name, "failed", err.Error())
		return
	}
	s.mu.Lock()
	jr := run.Jobs[name]
	jr.Status = "success"
	jr.Outputs = outputs
	run.Jobs[name] = jr
	run.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// Approve approves a waiting job and executes it.
func (s *WorkflowService) Approve(ctx context.Context, runID, jobName, approver string) error {
	s.mu.Lock()
	run, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		return domain.ErrNotFound
	}
	jr, ok := run.Jobs[jobName]
	if !ok {
		s.mu.Unlock()
		return domain.ErrNotFound
	}
	if jr.Status != "waiting_approval" {
		s.mu.Unlock()
		return fmt.Errorf("%w: job is not waiting for approval", domain.ErrValidation)
	}
	now := time.Now().UTC()
	jr.Status = "running"
	jr.ApprovedBy = approver
	jr.ApprovedAt = &now
	run.Jobs[jobName] = jr
	s.mu.Unlock()
	return nil
}

// Get returns a run by id.
func (s *WorkflowService) Get(runID string) (*domain.WorkflowRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	return run, ok
}

func (s *WorkflowService) evalBool(expression string, env map[string]any) (bool, error) {
	if s.evaluator == nil {
		return true, nil
	}
	out, err := s.evaluator.Eval(expression, env)
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

func (s *WorkflowService) finishRun(run *domain.WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "success"
	for _, jr := range run.Jobs {
		switch jr.Status {
		case "failed":
			status = "failed"
		case "waiting_approval", "pending", "running":
			if status != "failed" {
				status = "waiting_approval"
			}
		}
	}
	run.Status = status
	run.UpdatedAt = time.Now().UTC()
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
