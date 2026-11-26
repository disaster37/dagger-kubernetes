package domain

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ForgeKind identifies a git forge provider.
type ForgeKind string

const (
	ForgeGitHub ForgeKind = "github"
	ForgeGitLab ForgeKind = "gitlab"
	ForgeGitea  ForgeKind = "gitea"
)

// ForgeSource is a configured repository integration.
type ForgeSource struct {
	ID                      string    `json:"id"`
	OrgID                   string    `json:"org_id"`
	ProjectID               string    `json:"project_id,omitempty"`
	Provider                ForgeKind `json:"provider"`
	BaseURL                 string    `json:"base_url"`
	Repo                    string    `json:"repo"` // owner/repo or group/project
	AccountSourceID         string    `json:"account_source_id,omitempty"`
	TokenCiphertext         string    `json:"-"`
	WebhookSecretCiphertext string    `json:"-"`
	AppID                   string    `json:"app_id,omitempty"`
	InstallationID          string    `json:"installation_id,omitempty"`
	PrivateKeyCiphertext    string    `json:"-"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// ForgeSourceRepository persists forge integrations.
type ForgeSourceRepository interface {
	Upsert(ctx context.Context, s *ForgeSource) error
	Get(ctx context.Context, id string) (*ForgeSource, error)
	ListByOrg(ctx context.Context, orgID string) ([]*ForgeSource, error)
	Delete(ctx context.Context, id string) error
}

// ForgeCloneURL derives the HTTPS git clone URL for a repository from the
// provider's API base URL. It is idempotent: a repo that is already a full URL
// (or already carries the .git suffix) is returned unchanged.
func ForgeCloneURL(provider ForgeKind, baseURL, repo string) string {
	if repo == "" {
		return ""
	}
	if strings.Contains(repo, "://") {
		return repo
	}
	host := forgeHost(provider, baseURL)
	if host == "" {
		return repo
	}
	return fmt.Sprintf("https://%s/%s.git", host, strings.TrimSuffix(repo, ".git"))
}

// forgeHost extracts the git host from a provider API base URL. A missing base
// falls back to the provider's public SaaS host (github.com, gitlab.com,
// gitea.com) so the clone URL is never malformed; Gitea's public host is
// gitea.com (the canonical hosted instance).
func forgeHost(provider ForgeKind, baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	switch provider {
	case ForgeGitHub:
		if base == "" || base == "https://api.github.com" {
			return "github.com"
		}
		// GHES: https://<host>/api/v3 -> <host>
		return hostFromURL(strings.TrimSuffix(base, "/api/v3"))
	case ForgeGitLab:
		if base == "" {
			return "gitlab.com"
		}
		// https://<host>/api/v4 -> <host>
		return hostFromURL(strings.TrimSuffix(base, "/api/v4"))
	case ForgeGitea:
		if base == "" {
			return "gitea.com"
		}
		// https://<host>/api/v1 -> <host>
		return hostFromURL(strings.TrimSuffix(base, "/api/v1"))
	default:
		return ""
	}
}

// hostFromURL returns the host[:port] of a URL, falling back to the trimmed
// input when it is not a parseable URL.
func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// CanonicalForgeRepo returns the canonical "<host>/<owner>/<name>" form of a
// repository slug. A repo already carrying a host segment is returned with the
// scheme and .git suffix stripped; otherwise the provider's host derived from
// baseURL is prepended.
func CanonicalForgeRepo(provider ForgeKind, baseURL, repo string) string {
	repo = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(repo), "https://"), ".git")
	if repo == "" {
		return ""
	}
	parts := strings.Split(repo, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return repo
	}
	host := forgeHost(provider, baseURL)
	if host == "" {
		return repo
	}
	return fmt.Sprintf("%s/%s", host, repo)
}

// WebhookEvent is the normalized forge webhook payload.
type WebhookEvent struct {
	Provider      ForgeKind
	Event         string // push | tag | pull_request | release
	Action        string
	Repo          string
	SHA           string
	Ref           string
	DefaultBranch string
	TargetBranch  string
	Tag           string
	PRNumber      int
	SourceRepoID  string
	TargetRepoID  string
	IsFork        bool
	Release       string
}

// CheckState is a commit-status/check-run state.
type CheckState string

const (
	CheckPending CheckState = "pending"
	CheckSuccess CheckState = "success"
	CheckFailure CheckState = "failure"
	CheckError   CheckState = "error"
)

// CheckRun is one check result reported to a forge.
type CheckRun struct {
	Name       string
	HeadSHA    string
	State      CheckState
	Title      string
	Summary    string
	DetailsURL string
}

// ForgeProvider is the outbound forge API adapter (implemented in repository).
// RegisterWebhook/DeleteWebhook/SetCommitStatus/CreateCheckRun receive the
// PLAINTEXT credential (token) — never the sealed ciphertext stored on
// ForgeSource.
type ForgeProvider interface {
	Kind() ForgeKind
	RegisterWebhook(ctx context.Context, source *ForgeSource, token, callbackURL, secret string) (string, error)
	DeleteWebhook(ctx context.Context, source *ForgeSource, token, webhookID string) error
	VerifyWebhook(source *ForgeSource, secret string, headers map[string]string, body []byte) bool
	SetCommitStatus(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
	CreateCheckRun(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
}

// CheckItem is one discovered check and its live/terminal state.
type CheckItem struct {
	Name      string     `json:"name"`
	State     CheckState `json:"state"`
	TraceID   string     `json:"trace_id,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Tests is filled at read time from the check's trace (never persisted) so
	// GET /api/v1/checks/runs exposes per-test results.
	Tests []TestResult `json:"tests,omitempty"`
	// Filters carries the typed artifact columns discovered by
	// `dagger check -l --all` (header column -> value). Optional.
	Filters map[string]string `json:"filters,omitempty"`
}

// CheckRunRecord is the replicated Cloud Checks run state.
type CheckRunRecord struct {
	ID             string      `json:"id"`
	OrgID          string      `json:"org_id"`
	ProjectID      string      `json:"project_id"`
	SourceID       string      `json:"source_id"`
	Provider       ForgeKind   `json:"provider"`
	Repo           string      `json:"repo"`
	Event          string      `json:"event"`
	SHA            string      `json:"sha"`
	Ref            string      `json:"ref"`
	DefaultBranch  string      `json:"default_branch,omitempty"`
	TargetBranch   string      `json:"target_branch,omitempty"`
	Tag            string      `json:"tag,omitempty"`
	PRNumber       int         `json:"pr_number,omitempty"`
	IsFork         bool        `json:"is_fork"`
	IsRelease      bool        `json:"is_release"`
	Status         string      `json:"status"` // waiting_approval | running | success | failed | canceled
	IncludeSecrets bool        `json:"include_secrets"`
	ApprovedBy     string      `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time  `json:"approved_at,omitempty"`
	Checks         []CheckItem `json:"checks,omitempty"`
	// Filters holds the canonical "--<key>=<value>" typed-filter argv applied
	// to a rerun (e.g. "--go-test=TestGet"). Stored only on explicit rerun.
	Filters []string `json:"filters,omitempty"`
	// Artifacts names the typed artifacts the run's checks cover (load phase).
	Artifacts     []ArtifactRef     `json:"artifacts,omitempty"`
	WorkflowFiles map[string]string `json:"workflow_files,omitempty"` // raw .dagger/workflows/*.yaml captured in load phase
	ContextFiles  map[string][]byte `json:"context_files,omitempty"`  // <=1MiB structured files referenced by file()/next_semver()
	Error         string            `json:"error,omitempty"`
	RunTokenHash  string            `json:"-"` // SHA-256 of the per-run callback token; never exposed via API
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// CheckRunFilter filters CheckRunRecord listings.
type CheckRunFilter struct {
	OrgID     string
	ProjectID string
	Status    string
	Repo      string
	Limit     int
}

// CheckRunRepository persists Cloud Checks run records.
type CheckRunRepository interface {
	Upsert(ctx context.Context, r *CheckRunRecord) error
	Get(ctx context.Context, id string) (*CheckRunRecord, error)
	List(ctx context.Context, f *CheckRunFilter) ([]*CheckRunRecord, error)
	Delete(ctx context.Context, id string) error
}

// CheckRunSpec is the leader-side description of one check-run Job.
type CheckRunSpec struct {
	ID            string
	OrgID         string
	ProjectID     string
	SourceID      string
	Provider      ForgeKind
	Repo          string
	SHA           string
	Ref           string
	DefaultBranch string
	TargetBranch  string
	Tag           string
	PRNumber      int
	IsFork        bool
	IsRelease     bool
	CheckName     string
	TraceID       string
	Environment   string
	Token         string
	RunToken      string
	CloneToken    string
	CloneURL      string
	Event         string
	Secrets       map[string]string
	// Filters are the validated "--<key>=<value>" argv passed to `dagger check`.
	Filters []string
}

// CheckJobRunner executes check-run Jobs (implemented in repository).
type CheckJobRunner interface {
	Run(ctx context.Context, spec *CheckRunSpec) error
	CancelSuperseded(ctx context.Context, projectID, ref string) error
	JobStatus(ctx context.Context, runID string) (JobState, error)
}

// JobState is the observed state of a runner Job.
type JobState string

const (
	JobStateUnknown   JobState = "unknown"
	JobStateActive    JobState = "active"
	JobStateSucceeded JobState = "succeeded"
	JobStateFailed    JobState = "failed"
)

// CheckRunFilters is the semantic set of typed filters a caller may request on
// a rerun. Each entry is either an explicit "key=value" pair (module-specific,
// e.g. "vitest-test-file=app.test.ts") or a bare value that is mapped to the
// platform default key for its category.
type CheckRunFilters struct {
	Tests  []string `json:"tests,omitempty"`
	Files  []string `json:"files,omitempty"`
	Checks []string `json:"checks,omitempty"`
}

// ArtifactRef names one typed artifact a check covers. Typed artifacts
// (discovered by Dagger) are distinct from stored workflow artifacts.
type ArtifactRef struct {
	Kind    string            `json:"kind"`              // module column, e.g. "go", "vitest"
	Name    string            `json:"name,omitempty"`    // check name
	Filters map[string]string `json:"filters,omitempty"` // column -> value
	Link    string            `json:"link,omitempty"`    // dag:// link when the CLI provides one
}

// checkFilterRe is the strict charset for a typed filter: a lowercase
// key=value pair with no shell metacharacters and no leading "-" (which would
// be parsed as a flag).
var checkFilterRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}=[A-Za-z0-9_./:@+\[\]-]{1,200}$`)

// checkFilterPrefixes maps a semantic category to the default filter key used
// when the caller passes a bare value.
var checkFilterPrefixes = []struct {
	category string
	key      string
}{
	{category: "tests", key: "go-test"},
	{category: "files", key: "go-package"},
	{category: "checks", key: "check"},
}

// CheckFilterArgv validates one "key=value" typed filter and returns the
// canonical "--key=value" argv form. It fails with ErrValidation.
func CheckFilterArgv(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !checkFilterRe.MatchString(raw) {
		return "", invalidCheckFilter(raw)
	}
	return fmt.Sprintf("--%s", raw), nil
}

// ParseCheckFilterArgv validates a stored "--key=value" filter and returns the
// bare "key=value" form used by the runner's --filter flag.
func ParseCheckFilterArgv(arg string) (string, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(arg), "--")
	if !checkFilterRe.MatchString(raw) {
		return "", invalidCheckFilter(raw)
	}
	return raw, nil
}

// invalidCheckFilter is the shared validation error.
func invalidCheckFilter(raw string) error {
	return fmt.Errorf("%w: invalid check filter: %q must be key=value (lowercase key, no shell metacharacters, no leading dash)", ErrValidation, raw)
}

// NormalizeCheckFilters converts a semantic filter request into canonical
// "--key=value" argv entries, de-duplicated in request order.
func NormalizeCheckFilters(f CheckRunFilters) ([]string, error) {
	values := map[string][]string{
		"tests":  f.Tests,
		"files":  f.Files,
		"checks": f.Checks,
	}
	out := make([]string, 0, len(f.Tests)+len(f.Files)+len(f.Checks))
	seen := make(map[string]bool, cap(out))
	for _, p := range checkFilterPrefixes {
		for _, value := range values[p.category] {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, fmt.Errorf("%w: invalid check filter: empty %s entry", ErrValidation, p.category)
			}
			if !strings.Contains(value, "=") {
				value = fmt.Sprintf("%s=%s", p.key, value)
			}
			argv, err := CheckFilterArgv(value)
			if err != nil {
				return nil, err
			}
			if seen[argv] {
				continue
			}
			seen[argv] = true
			out = append(out, argv)
		}
	}
	return out, nil
}

// MergeCheckFilters validates the stored argv filters and unions them with a
// new semantic request. Stored filters keep their order; new entries are
// appended once. Validation happens before any mutation so a bad request never
// alters a record.
func MergeCheckFilters(stored []string, f CheckRunFilters) ([]string, error) {
	out := make([]string, 0, len(stored)+len(f.Tests)+len(f.Files)+len(f.Checks))
	seen := make(map[string]bool, cap(out))
	for _, arg := range stored {
		normalized, err := CheckFilterArgv(strings.TrimPrefix(strings.TrimSpace(arg), "--"))
		if err != nil {
			return nil, err
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	added, err := NormalizeCheckFilters(f)
	if err != nil {
		return nil, err
	}
	for _, argv := range added {
		if seen[argv] {
			continue
		}
		seen[argv] = true
		out = append(out, argv)
	}
	return out, nil
}
