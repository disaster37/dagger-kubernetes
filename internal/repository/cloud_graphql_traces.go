package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/graph-gophers/graphql-go"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// graphqlTime is the custom `Time` scalar used by the Cloud GraphQL schema. It
// marshals as an RFC3339Nano timestamp and unmarshals either an RFC3339Nano
// timestamp (used by TraceFilter `since`/`until`) or a base64url offset cursor
// (used by traces pagination `after`).
type graphqlTime struct {
	time   time.Time
	cursor string
}

// ImplementsGraphQLType implements graphql.Scalar.
func (graphqlTime) ImplementsGraphQLType(name string) bool { return name == "Time" }

// UnmarshalGraphQL accepts a time.Time, an RFC3339Nano string, or a base64url
// offset cursor. Anything else (including empty or oversized inputs) is
// rejected (CWE-20).
func (t *graphqlTime) UnmarshalGraphQL(input any) error {
	switch v := input.(type) {
	case time.Time:
		t.time = v
		return nil
	case string:
		return t.parse(v)
	case []byte:
		return t.parse(string(v))
	default:
		return fmt.Errorf("invalid Time value: unsupported type %T", input)
	}
}

func (t *graphqlTime) parse(raw string) error {
	if raw == "" || len(raw) > 128 {
		return fmt.Errorf("invalid Time value")
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		t.time = ts
		return nil
	}
	if offset, err := decodeTimeCursor(raw); err == nil {
		t.cursor = raw
		if offset >= 0 {
			t.time = time.Unix(int64(offset), 0).UTC()
		}
		return nil
	}
	return fmt.Errorf("invalid Time value")
}

// MarshalJSON implements graphql.Scalar.
func (t graphqlTime) MarshalJSON() ([]byte, error) {
	if t.time.IsZero() {
		return []byte("null"), nil
	}
	return []byte(strconv.Quote(t.time.UTC().Format(time.RFC3339Nano))), nil
}

// decodeTimeCursor decodes a base64url-encoded non-negative integer offset.
func decodeTimeCursor(raw string) (int, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(string(decoded))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return n, nil
}

// encodeTimeCursor encodes an offset as a base64url cursor (no padding).
func encodeTimeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// graphqlTraceFilter mirrors the TraceFilter SDL input.
type graphqlTraceFilter struct {
	Repos       *[]string
	Branch      *string
	Commit      *string
	Tag         *string
	Change      *string
	Status      *string
	Local       *bool
	Mine        *bool
	User        *string
	Token       *string
	Author      *string
	Name        *string
	Provider    *string
	Since       *graphqlTime
	Until       *graphqlTime
	MinDuration *float64
	MaxDuration *float64
}

// Query.Trace resolves one trace by ID, enforcing org access.
func (r *graphqlRoot) Trace(ctx context.Context, args struct {
	ID  graphql.ID
	Org *graphql.ID
}) (*graphqlTrace, error) {
	if _, err := r.requireAuthenticated(ctx); err != nil {
		return nil, err
	}
	if args.Org != nil {
		if _, err := r.resolveAccessibleOrg(ctx, string(*args.Org)); err != nil {
			return nil, err
		}
	}
	m, err := r.traces.Get(ctx, string(args.ID))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	id := graphqlIdentity(ctx)
	orgID := m.OrgID
	if orgID == "" || !r.canAccessOrg(ctx, id, orgID) {
		return nil, nil
	}
	tr := &graphqlTrace{meta: m, sender: r.usernameFor(ctx, m.UserID)}
	return tr, nil
}

// usernameFor resolves a userID to its username, returning "" when unknown.
func (r *graphqlRoot) usernameFor(ctx context.Context, userID string) string {
	if userID == "" || r.users == nil {
		return ""
	}
	u, err := r.users.Get(ctx, userID)
	if err != nil {
		return ""
	}
	return u.Username
}

// parseTraceSort validates and returns the requested TraceSort ("" = default).
func parseTraceSort(raw *string) (string, error) {
	if raw == nil {
		return "", nil
	}
	s := *raw
	if s != "" && s != "START" && s != "DURATION" {
		return "", fmt.Errorf("invalid sort %q", s)
	}
	return s, nil
}

// tracePageArgs holds the clamped page size, cursor offset, and timestamp
// cursor decoded from the traces args.
type tracePageArgs struct {
	limit     int
	offset    int
	afterTime time.Time
}

func parseTracePageArgs(first *int32, after *graphqlTime, sortMode string) (tracePageArgs, error) {
	out := tracePageArgs{limit: domain.DefaultTraceLimit}
	if first != nil && *first > 0 {
		out.limit = int(*first)
	}
	if out.limit > domain.MaxTraceLimit {
		out.limit = domain.MaxTraceLimit
	}
	if after == nil {
		return out, nil
	}
	if after.cursor != "" {
		if sortMode == "DURATION" {
			return out, fmt.Errorf("cursor pagination is not supported with DURATION sort")
		}
		if n, err := decodeTimeCursor(after.cursor); err == nil {
			out.offset = n
		}
		return out, nil
	}
	if !after.time.IsZero() {
		out.afterTime = after.time
	}
	return out, nil
}

// Org.Traces lists traces with filtering, sorting, and cursor pagination.
func (o *graphqlOrg) Traces(ctx context.Context, args struct {
	Filter *graphqlTraceFilter
	Sort   *string
	First  *int32
	After  *graphqlTime
}) (*graphqlTraceConnection, error) {
	sortMode, err := parseTraceSort(args.Sort)
	if err != nil {
		return nil, err
	}
	page, err := parseTracePageArgs(args.First, args.After, sortMode)
	if err != nil {
		return nil, err
	}

	rows, err := o.root.traces.List(ctx, &domain.TraceFilter{
		OrgID:             o.org.ID,
		IncludeUnassigned: true,
		Limit:             domain.MaxTraceLimit,
	})
	if err != nil {
		return nil, err
	}

	filtered, err := o.applyTraceFilter(ctx, rows, args.Filter, page.afterTime)
	if err != nil {
		return nil, err
	}
	if sortMode == "DURATION" {
		sortTracesByDuration(filtered)
	}

	offset := min(page.offset, len(filtered))
	end := min(offset+page.limit, len(filtered))
	pageRows := filtered[offset:end]
	nodes := make([]*graphqlTrace, 0, len(pageRows))
	for i := range pageRows {
		row := pageRows[i]
		nodes = append(nodes, &graphqlTrace{meta: &row.TraceMeta, sender: row.Username})
	}

	hasNext := sortMode != "DURATION" && len(filtered) > end
	var endCursor *string
	if hasNext {
		cursor := encodeTimeCursor(end)
		endCursor = &cursor
	}
	return &graphqlTraceConnection{nodes: nodes, hasNext: hasNext, endCursor: endCursor}, nil
}

// applyTraceFilter filters rows in memory and optionally drops rows at or
// before afterTime (timestamp-cursor semantics for START sort).
func (o *graphqlOrg) applyTraceFilter(ctx context.Context, rows []*domain.TraceListResult, f *graphqlTraceFilter, afterTime time.Time) ([]*domain.TraceListResult, error) {
	out := make([]*domain.TraceListResult, 0, len(rows))
	for _, row := range rows {
		key := traceSortKey(&row.TraceMeta)
		if !afterTime.IsZero() && !key.Before(afterTime) {
			continue
		}
		if f != nil {
			ok, err := o.traceMatchesFilter(ctx, row, f)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func (o *graphqlOrg) traceMatchesFilter(ctx context.Context, row *domain.TraceListResult, f *graphqlTraceFilter) (bool, error) {
	m := &row.TraceMeta
	if !traceStatusFilterMatches(f, m) {
		return false, nil
	}
	if !traceIdentityFilterMatches(ctx, f, row) {
		return false, nil
	}
	if !traceRepoFilterMatches(f, m) {
		return false, nil
	}
	return traceTimeFilterMatches(f, m)
}

func traceStatusFilterMatches(f *graphqlTraceFilter, m *domain.TraceMeta) bool {
	if f.Status == nil {
		return true
	}
	return traceStatusMatches(*f.Status, m.Status)
}

func traceIdentityFilterMatches(ctx context.Context, f *graphqlTraceFilter, row *domain.TraceListResult) bool {
	m := &row.TraceMeta
	if f.Local != nil && (m.CIProvider == "") != *f.Local {
		return false
	}
	if f.Mine != nil {
		isMine := graphqlIdentity(ctx).UserID == m.UserID
		if isMine != *f.Mine {
			return false
		}
	}
	if f.User != nil && *f.User != "" {
		if !strings.EqualFold(*f.User, m.UserID) && !strings.EqualFold(*f.User, row.Username) {
			return false
		}
	}
	if f.Provider != nil && *f.Provider != "" && !strings.EqualFold(*f.Provider, m.CIProvider) {
		return false
	}
	if f.Name != nil && *f.Name != "" && !matchTraceName(*f.Name, m.ProjectName) {
		return false
	}
	return true
}

func traceRepoFilterMatches(f *graphqlTraceFilter, m *domain.TraceMeta) bool {
	if f.Repos == nil || len(*f.Repos) == 0 {
		return true
	}
	return traceMatchesRepos(*f.Repos, m)
}

// traceTimeFilterMatches enforces since/until and min/max duration bounds.
// branch/commit/tag/change/token/author are not modelled by self-hosted trace
// metadata; they are accepted but ignored.
func traceTimeFilterMatches(f *graphqlTraceFilter, m *domain.TraceMeta) (bool, error) {
	if f.Since != nil {
		if f.Since.time.IsZero() {
			return false, fmt.Errorf("invalid since timestamp")
		}
		if traceSortKey(m).Before(f.Since.time) {
			return false, nil
		}
	}
	if f.Until != nil {
		if f.Until.time.IsZero() {
			return false, fmt.Errorf("invalid until timestamp")
		}
		if traceSortKey(m).After(f.Until.time) {
			return false, nil
		}
	}
	if f.MinDuration != nil && float64(m.DurationMS)/1000 < *f.MinDuration {
		return false, nil
	}
	if f.MaxDuration != nil && float64(m.DurationMS)/1000 > *f.MaxDuration {
		return false, nil
	}
	return true, nil
}

func traceStatusMatches(filter, status string) bool {
	switch strings.ToUpper(filter) {
	case "PASSED", "SUCCESS":
		return status == "success"
	case "FAILED", "ERROR":
		return status == "failed"
	case "RUNNING":
		return status == "" || status == "running" || status == "unset"
	default:
		return false
	}
}

func matchTraceName(pattern, name string) bool {
	lower := strings.ToLower(pattern)
	if strings.ContainsAny(lower, "*?[") {
		ok, err := path.Match(lower, strings.ToLower(name))
		return err == nil && ok
	}
	return strings.Contains(strings.ToLower(name), lower)
}

func traceMatchesRepos(repos []string, m *domain.TraceMeta) bool {
	for _, want := range repos {
		if containsFold(m.CIRepo, want) || containsFold(m.ProjectName, want) {
			return true
		}
	}
	return false
}

func sortTracesByDuration(rows []*domain.TraceListResult) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].DurationMS > rows[j].DurationMS
	})
}

// Org.LastUserTrace returns the caller's most recent trace in the org (or the
// newest org trace when the caller has none).
func (o *graphqlOrg) LastUserTrace(ctx context.Context) (*graphqlTrace, error) {
	rows, err := o.root.traces.List(ctx, &domain.TraceFilter{
		OrgID:             o.org.ID,
		IncludeUnassigned: true,
		Limit:             domain.MaxTraceLimit,
	})
	if err != nil || len(rows) == 0 {
		return nil, nil
	}
	callerID := graphqlIdentity(ctx).UserID
	if callerID != "" {
		for i := range rows {
			if rows[i].UserID == callerID {
				return &graphqlTrace{meta: &rows[i].TraceMeta, sender: rows[i].Username}, nil
			}
		}
	}
	return &graphqlTrace{meta: &rows[0].TraceMeta, sender: rows[0].Username}, nil
}

// graphqlTrace is the Trace GraphQL object type.
type graphqlTrace struct {
	meta   *domain.TraceMeta
	sender string
}

func (t *graphqlTrace) ID() graphql.ID  { return graphql.ID(t.meta.TraceID) }
func (t *graphqlTrace) Name() string    { return t.meta.ProjectName }
func (t *graphqlTrace) Project() string { return t.meta.ProjectName }
func (t *graphqlTrace) Version() string { return t.meta.Version }
func (t *graphqlTrace) DurationMS() int32 {
	if t.meta.DurationMS > int64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	return int32(t.meta.DurationMS) // #nosec G115 -- clamped above
}
func (t *graphqlTrace) StartedAt() string { return t.meta.StartedAt.UTC().Format(time.RFC3339) }
func (t *graphqlTrace) UpdatedAt() string { return t.meta.UpdatedAt.UTC().Format(time.RFC3339) }
func (t *graphqlTrace) Timestamp() graphqlTime {
	return graphqlTime{time: traceSortKey(t.meta)}
}

func (t *graphqlTrace) Status() *graphqlTraceStatus {
	switch t.meta.Status {
	case "", "running", "unset":
		return nil
	case "success":
		return &graphqlTraceStatus{code: "STATUS_CODE_OK"}
	default:
		return &graphqlTraceStatus{code: "STATUS_CODE_ERROR", message: t.meta.FailureReason}
	}
}

func (t *graphqlTrace) EndTime() *graphqlTime {
	switch t.meta.Status {
	case "", "running", "unset":
		return nil
	default:
		end := traceSortKey(t.meta).Add(time.Duration(t.meta.DurationMS) * time.Millisecond)
		return &graphqlTime{time: end}
	}
}

func (t *graphqlTrace) Local() bool { return t.meta.CIProvider == "" }

func (t *graphqlTrace) Sender() *graphqlTraceSender {
	if t.sender == "" {
		return nil
	}
	return &graphqlTraceSender{name: t.sender}
}

func (t *graphqlTrace) Git() *graphqlTraceGit { return nil }

func (t *graphqlTrace) Ci() *graphqlTraceCI {
	if t.meta.CIProvider == "" && t.meta.CIRepo == "" {
		return nil
	}
	ci := &graphqlTraceCI{isNativeCI: t.meta.CIProvider == "dagger-checks"}
	if t.meta.CIProvider != "" {
		provider := t.meta.CIProvider
		ci.provider = &provider
	}
	if t.meta.CIRepo != "" {
		repo := t.meta.CIRepo
		ci.repository = &repo
	}
	return ci
}

// graphqlTraceStatus is the TraceStatus object type.
type graphqlTraceStatus struct {
	code    string
	message string
}

func (s *graphqlTraceStatus) Code() string    { return s.code }
func (s *graphqlTraceStatus) Message() string { return s.message }

type graphqlTraceSender struct{ name string }

func (s *graphqlTraceSender) Name() string { return s.name }

type graphqlTraceGit struct {
	remote string
	title  string
	ref    string
	tag    *string
	branch *string
	author *graphqlTraceGitAuthor
}

func (g *graphqlTraceGit) Remote() string                 { return g.remote }
func (g *graphqlTraceGit) Title() string                  { return g.title }
func (g *graphqlTraceGit) Ref() string                    { return g.ref }
func (g *graphqlTraceGit) Tag() *string                   { return g.tag }
func (g *graphqlTraceGit) Branch() *string                { return g.branch }
func (g *graphqlTraceGit) Author() *graphqlTraceGitAuthor { return g.author }

type graphqlTraceGitAuthor struct {
	name  string
	email string
}

func (a *graphqlTraceGitAuthor) Name() string  { return a.name }
func (a *graphqlTraceGitAuthor) Email() string { return a.email }

type graphqlTraceCI struct {
	isNativeCI bool
	provider   *string
	repository *string
	change     *graphqlTraceCIChange
}

func (c *graphqlTraceCI) IsNativeCI() bool              { return c.isNativeCI }
func (c *graphqlTraceCI) Provider() *string             { return c.provider }
func (c *graphqlTraceCI) Repository() *string           { return c.repository }
func (c *graphqlTraceCI) Change() *graphqlTraceCIChange { return c.change }

type graphqlTraceCIChange struct {
	id      string
	title   string
	branch  string
	headSHA string
}

func (c *graphqlTraceCIChange) ID() string      { return c.id }
func (c *graphqlTraceCIChange) Title() string   { return c.title }
func (c *graphqlTraceCIChange) Branch() string  { return c.branch }
func (c *graphqlTraceCIChange) HeadSHA() string { return c.headSHA }

type graphqlTraceConnection struct {
	nodes     []*graphqlTrace
	hasNext   bool
	endCursor *string
}

func (c *graphqlTraceConnection) Nodes() []*graphqlTrace { return c.nodes }
func (c *graphqlTraceConnection) PageInfo() *graphqlPageInfo {
	return &graphqlPageInfo{hasNext: c.hasNext, endCursor: c.endCursor}
}

type graphqlPageInfo struct {
	hasNext   bool
	endCursor *string
}

func (p *graphqlPageInfo) HasNextPage() bool  { return p.hasNext }
func (p *graphqlPageInfo) EndCursor() *string { return p.endCursor }
