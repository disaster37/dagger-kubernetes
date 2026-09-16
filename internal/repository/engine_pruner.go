package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	enginePrunePort    = enginePort       // the engine's session HTTP port (plaintext; see k8s_provider)
	enginePruneTimeout = 10 * time.Minute // hard backstop; the caller's ctx is the primary bound

	// maxPruneErrorBody caps how much of a failing engine response is embedded
	// in an error (diagnosability without unbounded error strings).
	maxPruneErrorBody = 1 << 10 // 1 KiB
)

// pruneQuery is the raw dagql mutation the Dagger CLI issues for
// `dagger core engine local-cache prune --use-default-policy=false`.
const pruneQuery = "{ engine { localCache { prune(useDefaultPolicy: false) } } }"

// engineClientMetadata is the minimal Dagger engine.ClientMetadata the engine
// reads from the X-Dagger-Client-Metadata header (engine/opts.go). It is
// JSON-marshalled then base64-encoded, exactly as the Dagger CLI does.
type engineClientMetadata struct {
	ClientID          string            `json:"client_id"`
	ClientSecretToken string            `json:"client_secret_token"`
	SessionID         string            `json:"session_id"`
	ClientHostname    string            `json:"client_hostname"`
	ClientStableID    string            `json:"client_stable_id"`
	ClientVersion     string            `json:"client_version"`
	Labels            map[string]string `json:"labels"`
}

// pruneGraphQLResponse is the subset of a GraphQL response the pruner inspects:
// a non-empty errors array means the mutation failed.
type pruneGraphQLResponse struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// SessionEnginePruner prunes a running engine pod's BuildKit local cache by
// speaking the engine's session HTTP protocol directly over plaintext TCP —
// the same wire call the Dagger CLI makes — instead of shelling out to the CLI.
type SessionEnginePruner struct {
	client *http.Client
	port   int // engine session port (9999 in production; tests override it)
}

var _ domain.EnginePruner = (*SessionEnginePruner)(nil)

// NewSessionEnginePruner returns a pruner that dials the engine session port on
// each pod IP over plaintext HTTP.
func NewSessionEnginePruner() *SessionEnginePruner {
	return &SessionEnginePruner{client: newPruneHTTPClient(), port: enginePrunePort}
}

// newPruneHTTPClient builds the one-shot HTTP client used for prunes. Keep-alives
// are disabled so a socket is never held open to a pod that is later
// scaled/deleted; the engine tears the synthetic session down when the
// connection closes.
func newPruneHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,  // bypass HTTP_PROXY: the engine is a pod IP
			DisableKeepAlives: true, // one-shot admin op; no conn reuse
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			},
		},
		// The engine answers directly; never chase redirects (CWE-601) — a 3xx
		// would silently re-dial another host carrying the session metadata.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: enginePruneTimeout,
	}
}

// PruneLocalCache implements domain.EnginePruner. It POSTs the raw dagql
// `{ engine { localCache { prune(useDefaultPolicy: false) } } }` to
// http://podIP:port/query with a fresh synthetic session, and treats any
// non-200 status or non-empty GraphQL errors array as a failure.
func (p *SessionEnginePruner) PruneLocalCache(ctx context.Context, podIP, version string) error {
	meta, err := newEngineClientMetadata(version)
	if err != nil {
		return fmt.Errorf("prune %s: %w", podIP, err)
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("prune %s: marshal client metadata: %w", podIP, err)
	}
	body, err := json.Marshal(map[string]string{"query": pruneQuery})
	if err != nil {
		return fmt.Errorf("prune %s: marshal query: %w", podIP, err)
	}

	url := fmt.Sprintf("http://%s:%d/query", podIP, p.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("prune %s: build request: %w", podIP, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dagger-Client-Metadata", base64.StdEncoding.EncodeToString(metaBytes))

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("prune %s: %w", podIP, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("prune %s: read response: %w", podIP, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prune %s: unexpected status %d: %s", podIP, resp.StatusCode, truncatePruneBody(respBody))
	}

	var gql pruneGraphQLResponse
	if err := json.Unmarshal(respBody, &gql); err != nil {
		return fmt.Errorf("prune %s: decode response: %w", podIP, err)
	}
	if len(gql.Errors) > 0 {
		return fmt.Errorf("prune %s: %s", podIP, gql.Errors[0].Message)
	}
	return nil
}

// newEngineClientMetadata builds a fresh synthetic session identity. The engine
// creates the session lazily from this metadata and makes its first client the
// main client, which engine.localCache.prune requires.
func newEngineClientMetadata(version string) (engineClientMetadata, error) {
	ids := make([]string, 4)
	for i := range ids {
		id, err := newPruneID()
		if err != nil {
			return engineClientMetadata{}, err
		}
		ids[i] = id
	}
	return engineClientMetadata{
		ClientID:          ids[0],
		ClientSecretToken: ids[1],
		SessionID:         ids[2],
		ClientStableID:    ids[3],
		ClientHostname:    "supervisor",
		ClientVersion:     version,
		Labels:            map[string]string{},
	}, nil
}

// newPruneID returns a fresh 32-char hex id (16 random bytes). It is named
// distinctly from the service package's newID and the repository test helper's
// newID to avoid a duplicate declaration in test builds.
func newPruneID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// truncatePruneBody bounds an engine error body embedded in an error string.
func truncatePruneBody(b []byte) string {
	if len(b) <= maxPruneErrorBody {
		return string(b)
	}
	return fmt.Sprintf("%s... (truncated)", b[:maxPruneErrorBody])
}
