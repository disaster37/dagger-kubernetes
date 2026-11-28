package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeS3 is a minimal in-memory S3 endpoint: single PUT/GET/HEAD/DELETE under
// one bucket, enough for the remote-cache store's PutObject/GetObject calls
// (presigning itself never touches the network).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	return &fakeS3{objects: map[string][]byte{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("location") {
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint>us-east-1</LocationConstraint>`)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	key = strings.TrimPrefix(key, "bucket/")
	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
			data = decodeAWSChunked(data)
		}
		f.mu.Lock()
		f.objects[key] = data
		f.mu.Unlock()
		w.Header().Set("ETag", `"fake"`)
	case http.MethodGet, http.MethodHead:
		f.mu.Lock()
		data, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"fake"`)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// decodeAWSChunked strips the streaming-signature framing minio-go uses for
// PutObject: "<hexsize>;chunk-signature=<hex>\r\n<data>\r\n", ending with a
// zero-size chunk.
func decodeAWSChunked(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		nl := bytes.Index(data, []byte("\r\n"))
		if nl < 0 {
			break
		}
		header := string(data[:nl])
		data = data[nl+2:]
		sizeHex, _, _ := strings.Cut(header, ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil || size == 0 || int(size) > len(data) {
			break
		}
		out = append(out, data[:size]...)
		data = data[size:]
		if len(data) >= 2 {
			data = data[2:]
		}
	}
	return out
}

func (f *fakeS3) hasPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// engineConn is a fake beta.28 engine: it sends requests with its own ID space
// and matches replies by Re.
type engineConn struct {
	t    *testing.T
	conn *websocket.Conn
	id   uint64
}

func (e *engineConn) send(typ string, id, re uint64, body any) {
	e.t.Helper()
	env, err := domain.NewRCEnvelope(id, re, typ, body)
	if err != nil {
		e.t.Fatalf("encode %s: %v", typ, err)
	}
	raw, err := env.Marshal()
	if err != nil {
		e.t.Fatalf("marshal %s: %v", typ, err)
	}
	if err := e.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		e.t.Fatalf("write %s: %v", typ, err)
	}
}

func (e *engineConn) next() uint64 {
	e.id++
	return e.id
}

func (e *engineConn) recv() domain.RCEnvelope {
	e.t.Helper()
	_ = e.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := e.conn.ReadMessage()
	if err != nil {
		e.t.Fatalf("read frame: %v", err)
	}
	env, err := domain.DecodeRCEnvelope(raw)
	if err != nil {
		e.t.Fatalf("decode frame %s: %v", raw, err)
	}
	return env
}

// recvType reads frames until one of typ arrives, failing on timeout.
func (e *engineConn) recvType(typ string) domain.RCEnvelope {
	e.t.Helper()
	seen := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		env := e.recv()
		if env.Type == typ {
			return env
		}
		seen = append(seen, env.Type)
	}
	e.t.Fatalf("no %q frame received (saw %v)", typ, seen)
	return domain.RCEnvelope{}
}

func (e *engineConn) reply(to domain.RCEnvelope, typ string, body any) {
	e.t.Helper()
	e.send(typ, e.next(), to.ID, body)
}

func TestRemoteCacheBeta28FakeEngine(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)
	orgsSvc, orgID := newIntegrationOrgService(t, store, groupRepo, tokensSvc, logger)
	orgToken, _, err := tokensSvc.GenerateFor(context.Background(), "org:"+orgID, "dag_default_")
	if err != nil {
		t.Fatalf("mint org token: %v", err)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	authSvc.SetOrgRepository(repository.NewOrgRepo(store))

	index, err := repository.OpenBboltCacheIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	s3 := newFakeS3(t)
	s3srv := httptest.NewServer(s3)
	t.Cleanup(s3srv.Close)
	endpoint := strings.TrimPrefix(s3srv.URL, "http://")
	remoteStore, err := repository.NewS3RemoteCacheStore(endpoint, "us-east-1", "key", "secret", false, endpoint, "bucket", "remote-cache", time.Hour)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	remoteCache := service.NewRemoteCacheService(index, remoteStore, time.Hour, 0, 4, 0, time.Hour, observ.NewMetrics(nil), logger)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr := listenerAddr(controlLn)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        listenerAddr(dataLn),
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
		FleetIsolation:  "per_org",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Orgs: orgsSvc, Tokens: tokensSvc, JWT: jwtSvc, RemoteCache: remoteCache,
		TraceMeta: traceMetaRepo,
	})
	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(200 * time.Millisecond)

	layer := "sha256:" + strings.Repeat("c", 64)
	recipe := "sha256:" + strings.Repeat("d", 64)
	bundle := json.RawMessage(`{"version":1,"roots":[{"ordinal":1}],"values":[{"ordinal":1,"senderNumber":7,"record":{"resultID":7}}],"outputs":[],"layers":[{"descriptor":{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + layer + `","size":3}}]}`)

	dial := func(readOnly bool) *engineConn {
		headers := http.Header{}
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(orgToken+":")))
		if readOnly {
			headers.Set("X-Dagger-Cache-Mode", "read-only")
		}
		conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost"+controlAddr+"/v1/engine", headers)
		if err != nil {
			t.Fatalf("dial /v1/engine: %v", err)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Cleanup(func() { _ = conn.Close() })
		return &engineConn{t: t, conn: conn}
	}

	// --- read-write connection: hello → welcome → settle → export/upload ---
	eng := dial(false)
	eng.send(domain.RCTypeHello, 1, 0, domain.Hello{
		CacheID: "cache-x", Generation: 1, EngineVersion: "v1.0.0-beta.28", EngineName: "engine-x", Period: 1,
	})
	welcome := eng.recvType(domain.RCTypeWelcome)
	if welcome.Re != 1 {
		t.Fatalf("welcome re = %d, want 1", welcome.Re)
	}

	// Telemetry would have reported the settled period's entries; feed the
	// exact reference the ingestion path records.
	remoteCache.RecordCacheRoots([]service.CacheRootRef{{CacheID: "cache-x", Number: 7, Recipe: recipe}})

	eng.send(domain.RCTypeSettle, 2, 0, domain.Settle{Period: 1, Sessions: []domain.SettleSession{{ID: "sess-1", Marker: true}}})
	settled := eng.recvType(domain.RCTypeSettled)
	if settled.Re != 2 {
		t.Fatalf("settled re = %d, want 2", settled.Re)
	}

	export := eng.recvType(domain.RCTypeExport)
	var exportBody domain.Export
	if err := export.DecodeBody(&exportBody); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(exportBody.Roots) != 1 || exportBody.Roots[0] != 7 {
		t.Fatalf("export roots = %v, want [7]", exportBody.Roots)
	}
	eng.reply(export, domain.RCTypeExported, domain.Exported{ExportID: "exp-1", Bundle: bundle})

	upload := eng.recvType(domain.RCTypeUpload)
	var uploadBody domain.Upload
	if err := upload.DecodeBody(&uploadBody); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if uploadBody.ExportID != "exp-1" || !uploadBody.Merged {
		t.Fatalf("upload = %+v, want exp-1/merged", uploadBody)
	}
	// Small layers travel in one dagger-pack v1 object; the engine writes the
	// header and members to the presigned pack PUT.
	if len(uploadBody.Packs) != 1 || len(uploadBody.Packs[0].Members) != 1 || uploadBody.Packs[0].Members[0].Digest != layer {
		t.Fatalf("upload = %+v, want one pack carrying %s", uploadBody, layer)
	}
	eng.reply(upload, domain.RCTypeUploaded, domain.Uploaded{Done: []string{uploadBody.Packs[0].Pack}})

	if !s3.hasPrefix(fmt.Sprintf("orgs/%s/remote-cache/bundles/", orgID)) {
		t.Fatalf("no bundle stored under orgs/%s/remote-cache/", orgID)
	}

	// --- same connection: lookup hits the stored bundle and is answered by a
	// merge that names the lookup ---
	eng.send(domain.RCTypeLookup, 3, 0, domain.Lookup{Misses: []domain.LookupMiss{{Recipe: recipe, Field: "Container.from"}}})
	lookedUp := eng.recvType(domain.RCTypeLookedUp)
	if lookedUp.Re != 3 {
		t.Fatalf("lookedUp re = %d, want 3", lookedUp.Re)
	}
	var lookedUpBody domain.LookedUp
	if err := lookedUp.DecodeBody(&lookedUpBody); err != nil {
		t.Fatalf("decode lookedUp: %v", err)
	}
	if len(lookedUpBody.Misses) != 1 || lookedUpBody.Misses[0].Status != domain.RCLookupMerging {
		t.Fatalf("lookedUp = %+v, want one merging miss", lookedUpBody.Misses)
	}
	merge := eng.recvType(domain.RCTypeMerge)
	var mergeBody domain.Merge
	if err := merge.DecodeBody(&mergeBody); err != nil {
		t.Fatalf("decode merge: %v", err)
	}
	if merge.ID != lookedUpBody.Misses[0].Merge {
		t.Fatalf("merge id = %d, want the LookedUpMiss.Merge id %d", merge.ID, lookedUpBody.Misses[0].Merge)
	}
	if mergeBody.Lookup != 3 {
		t.Fatalf("merge lookup = %d, want the engine lookup id 3", mergeBody.Lookup)
	}
	if !strings.Contains(string(mergeBody.Bundle), `"senderNumber":7`) {
		t.Fatalf("merge bundle = %s, want the stored bundle", mergeBody.Bundle)
	}
	eng.reply(merge, domain.RCTypeMerged, domain.Merged{Generation: 1, EngineTime: time.Now().UnixNano(), Values: []json.RawMessage{}, Retained: []json.RawMessage{}, Stale: []json.RawMessage{}})

	// --- read-only connection: welcome, but never export or merge ---
	ro := dial(true)
	ro.send(domain.RCTypeHello, 1, 0, domain.Hello{CacheID: "cache-x", Generation: 1, Period: 1, Settled: true})
	if welcome := ro.recvType(domain.RCTypeWelcome); welcome.Re != 1 {
		t.Fatalf("read-only welcome re = %d, want 1", welcome.Re)
	}
	ro.send(domain.RCTypeLookup, 2, 0, domain.Lookup{Misses: []domain.LookupMiss{{Recipe: recipe}}})
	roLookedUp := ro.recvType(domain.RCTypeLookedUp)
	var roBody domain.LookedUp
	if err := roLookedUp.DecodeBody(&roBody); err != nil {
		t.Fatalf("decode read-only lookedUp: %v", err)
	}
	if len(roBody.Misses) != 1 || roBody.Misses[0].Status != domain.RCLookupNotStored {
		t.Fatalf("read-only lookedUp = %+v, want notStored", roBody.Misses)
	}
}

// TestRemoteCacheBeta28DuplicateHello proves a second hello on one connection
// is answered with an error and never re-welcomed.
func TestRemoteCacheBeta28DuplicateHello(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	usersSvc := service.NewUserService(repository.NewUserRepo(store), groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, repository.NewUserRepo(store), logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)
	orgsSvc, orgID := newIntegrationOrgService(t, store, groupRepo, tokensSvc, logger)
	orgToken, _, err := tokensSvc.GenerateFor(context.Background(), "org:"+orgID, "dag_default_")
	if err != nil {
		t.Fatalf("mint org token: %v", err)
	}
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	authSvc.SetOrgRepository(repository.NewOrgRepo(store))

	index, err := repository.OpenBboltCacheIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	s3srv := httptest.NewServer(newFakeS3(t))
	t.Cleanup(s3srv.Close)
	endpoint := strings.TrimPrefix(s3srv.URL, "http://")
	remoteStore, err := repository.NewS3RemoteCacheStore(endpoint, "us-east-1", "k", "s", false, endpoint, "bucket", "remote-cache", time.Hour)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	remoteCache := service.NewRemoteCacheService(index, remoteStore, time.Hour, 0, 2, 0, time.Hour, observ.NewMetrics(nil), logger)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	fleetManager := service.NewManager(repository.NewStubProvider(), sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 1, MaxSessionsPerReplica: 1, ReplicaIdleTTL: time.Minute,
	}, logger, observ.NewMetrics(nil))

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr := listenerAddr(controlLn)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr, DataAddr: listenerAddr(dataLn),
		ControlListener: controlLn, DataListener: dataLn, DataHost: "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Orgs: orgsSvc, Tokens: tokensSvc, JWT: jwtSvc, RemoteCache: remoteCache,
	})
	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(200 * time.Millisecond)

	headers := http.Header{}
	headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(orgToken+":")))
	conn, resp, err := websocket.DefaultDialer.Dial("ws://localhost"+controlAddr+"/v1/engine", headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	eng := &engineConn{t: t, conn: conn}
	eng.send(domain.RCTypeHello, 1, 0, domain.Hello{CacheID: "cache-y", Period: 1})
	if got := eng.recvType(domain.RCTypeWelcome); got.Re != 1 {
		t.Fatalf("welcome re = %d", got.Re)
	}
	eng.send(domain.RCTypeHello, 2, 0, domain.Hello{CacheID: "cache-y", Period: 1})
	errEnv := eng.recvType(domain.RCTypeError)
	if errEnv.Re != 2 {
		t.Fatalf("second hello re = %d, want error reply to 2", errEnv.Re)
	}
	var body domain.RemoteCacheError
	if err := errEnv.DecodeBody(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Message != "already welcomed" {
		t.Fatalf("error message = %q, want already welcomed", body.Message)
	}
}
