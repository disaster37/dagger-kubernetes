package pipeline

import (
	"fmt"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

// Supported returns the CI systems this library can render.
func Supported() []ci.CI { return []ci.CI{ci.Github, ci.Jenkins, ci.Gitlab} }

// CredentialConfig collects the CI-specific credential overrides provided by
// Dagger GenerateCi functions.
type CredentialConfig struct {
	RegistryUsernameKey   string // GitHub: override registry-username secret name
	RegistryPasswordKey   string // GitHub: override registry-password secret name
	RegistryCredential    string // Jenkins: override registry credential ID
	GitTokenCredential    string // Jenkins: override git-token credential ID
	RegistryUsernameVar   string // GitLab: override registry-username variable
	RegistryPasswordVar   string // GitLab: override registry-password variable
	GitTokenVar           string // GitLab: override git-token variable
}

// ResolveCredentialBindings maps CI-type and credential overrides to the
// Binding entries used in PipelineSpec.Job.Placeholders. This is the shared
// logic used by every module's GenerateCi to avoid duplicating the CI switch.
func ResolveCredentialBindings(ciType ci.CI, cfg CredentialConfig) (registryUser, registryPass, gitToken Binding, err error) {
	switch ciType {
	case ci.Github:
		ruRef := cfg.RegistryUsernameKey
		if ruRef == "" {
			ruRef = "GITHUB_TOKEN"
		}
		rpRef := cfg.RegistryPasswordKey
		if rpRef == "" {
			rpRef = "GITHUB_TOKEN"
		}
		return Binding{Kind: BindingSecret, Ref: ruRef},
			Binding{Kind: BindingSecret, Ref: rpRef},
			Binding{Kind: BindingSecret, Ref: "GITHUB_TOKEN"}, nil

	case ci.Jenkins:
		credRef := cfg.RegistryCredential
		if credRef == "" {
			credRef = "REGISTRY_CREDENTIAL"
		}
		gitRef := cfg.GitTokenCredential
		if gitRef == "" {
			gitRef = "GIT_TOKEN"
		}
		return Binding{Kind: BindingSecret, Ref: credRef},
			Binding{Kind: BindingSecret, Ref: credRef},
			Binding{Kind: BindingSecret, Ref: gitRef}, nil

	case ci.Gitlab:
		ruVar := cfg.RegistryUsernameVar
		if ruVar == "" {
			ruVar = "REGISTRY_USERNAME"
		}
		rpVar := cfg.RegistryPasswordVar
		if rpVar == "" {
			rpVar = "REGISTRY_PASSWORD"
		}
		gtVar := cfg.GitTokenVar
		if gtVar == "" {
			gtVar = "GIT_TOKEN"
		}
		return Binding{Kind: BindingSecret, Ref: ruVar},
			Binding{Kind: BindingSecret, Ref: rpVar},
			Binding{Kind: BindingSecret, Ref: gtVar}, nil

	default:
		return Binding{}, Binding{}, Binding{}, fmt.Errorf("unsupported CI: %s", ciType)
	}
}
