package service

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// LegacyImportResult summarizes a flat-file token import run.
type LegacyImportResult struct {
	Imported  int
	Skipped   int
	Usernames []string
}

// ImportTokensFile imports each line of the flat-file tokens file as a real
// user (legacy-N) with that exact token as its API token, member of an
// auto-created "legacy" group. Idempotent: tokens already present (by hash)
// are skipped. dryRun reports what would happen without writing.
func ImportTokensFile(ctx context.Context, path string, users *UserService, tokens *TokenService, groups *GroupService, logger *logrus.Logger, dryRun bool) (*LegacyImportResult, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is admin-configured, not user input.
	if err != nil {
		return nil, fmt.Errorf("read tokens file %s: %w", path, err)
	}

	// Ensure the "legacy" group exists (ignore "already exists" errors).
	var legacyGroup *domain.Group
	if !dryRun {
		g, err := groups.Create(ctx, GroupInput{Name: "legacy", AgentAvailable: true, MaxRunnerSessions: 0})
		if err != nil {
			// Likely duplicate; fetch instead.
			g, err = groups.GetByName(ctx, "legacy")
			if err != nil {
				return nil, fmt.Errorf("ensure legacy group: %w", err)
			}
		}
		legacyGroup = g
	}

	result := &LegacyImportResult{}
	idx := 0
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx++
		username := fmt.Sprintf("legacy-%d", idx)

		// Idempotency: skip if the token already validates (hash present).
		if _, err := tokens.Validate(ctx, line); err == nil {
			result.Skipped++
			continue
		}

		if dryRun {
			result.Imported++
			result.Usernames = append(result.Usernames, username)
			continue
		}

		// Create the user with a random (never-disclosed) password.
		randomPW := newPlaintextToken()
		u, err := users.Create(ctx, username, randomPW, domain.RoleUser)
		if err != nil {
			logger.WithError(err).WithField("username", username).Warn("legacy import: create user failed")
			result.Skipped++
			continue
		}
		if err := tokens.ImportRaw(ctx, u.ID, line); err != nil {
			logger.WithError(err).WithField("username", username).Warn("legacy import: import token failed")
			result.Skipped++
			continue
		}
		if legacyGroup != nil {
			if err := addGroupMember(ctx, groups, legacyGroup.ID, u.ID); err != nil {
				logger.WithError(err).WithField("username", username).Warn("legacy import: group membership failed")
			}
		}
		result.Imported++
		result.Usernames = append(result.Usernames, username)
	}

	logger.WithFields(logrus.Fields{
		"imported": result.Imported,
		"skipped":  result.Skipped,
	}).Info("legacy token import complete")
	return result, nil
}
