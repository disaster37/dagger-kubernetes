package ci

type CI string

const (
	// Github CI
	Github CI = "github"

	// Jenkins CI
	Jenkins CI = "jenkins"

	// GitLab CI
	Gitlab CI = "gitlab"

	// Default CI
	Dagger CI = "dagger"
)
