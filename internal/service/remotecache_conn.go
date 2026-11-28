package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// remoteCacheRequestTimeout bounds one service-initiated request (export,
// upload, merge) so a slow engine cannot pin the connection forever.
const remoteCacheRequestTimeout = 30 * time.Second

// errRemoteCacheConnClosed is returned to requests when the connection ends.
var errRemoteCacheConnClosed = errors.New("remote cache connection closed")

// RemoteCacheConn is the per-connection beta.28 protocol state machine. It has
// two independent message ID spaces (one per direction), a pending map keyed by
// the engine's request ID (Re), and the hello/settle state.
type RemoteCacheConn struct {
	svc       *RemoteCacheService
	namespace string
	readOnly  bool
	send      func([]byte) error
	logger    *logrus.Logger

	lastID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan domain.RCEnvelope
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup

	stateMu     sync.Mutex
	cacheID     string
	generation  uint64
	period      uint64
	welcomed    bool
	settled     bool
	merged      map[string]bool
	lastSettled uint64
}

func newRemoteCacheConn(svc *RemoteCacheService, namespace string, readOnly bool, send func([]byte) error) *RemoteCacheConn {
	return &RemoteCacheConn{
		svc:       svc,
		namespace: namespace,
		readOnly:  readOnly,
		send:      send,
		logger:    svc.logger,
		pending:   make(map[uint64]chan domain.RCEnvelope),
		done:      make(chan struct{}),
		merged:    make(map[string]bool),
	}
}

// Close ends the connection: pending requests fail and no further frame is
// dispatched.
func (c *RemoteCacheConn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.done)
	for id := range c.pending {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// HandleFrame decodes and routes one wire message: a reply (Re != 0) goes to
// its request, any other message is a request dispatched in the background so
// the read pump keeps draining.
func (c *RemoteCacheConn) HandleFrame(ctx context.Context, raw []byte) {
	env, err := domain.DecodeRCEnvelope(raw)
	if err != nil {
		c.logger.WithError(err).Debug("remote cache: undecodable frame")
		return
	}
	if env.Re != 0 {
		c.deliver(env)
		return
	}
	if env.Type == domain.RCTypeError {
		var body domain.RemoteCacheError
		_ = env.DecodeBody(&body)
		c.logger.WithField("message", body.Message).Debug("remote cache: engine error")
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.dispatch(ctx, env)
	}()
}

// deliver hands a reply to the request waiting on its ID.
func (c *RemoteCacheConn) deliver(env domain.RCEnvelope) {
	c.mu.Lock()
	ch := c.pending[env.Re]
	delete(c.pending, env.Re)
	c.mu.Unlock()
	if ch == nil {
		c.logger.WithFields(logrus.Fields{"re": env.Re, "type": env.Type}).Debug("remote cache: reply for unknown request")
		return
	}
	select {
	case ch <- env:
	default:
	}
}

// dispatch routes one engine request.
func (c *RemoteCacheConn) dispatch(ctx context.Context, env domain.RCEnvelope) {
	switch env.Type {
	case domain.RCTypeHello:
		c.handleHello(env)
	case domain.RCTypeOpen:
		c.handleOpen(env)
	case domain.RCTypeClaims:
		c.handleClaims(env)
	case domain.RCTypeSettle:
		c.handleSettle(env)
	case domain.RCTypeLookup:
		c.handleLookup(ctx, env)
	case domain.RCTypeRenew:
		c.handleRenew(ctx, env)
	default:
		_ = c.replyError(env, fmt.Sprintf("unexpected request %q", env.Type))
	}
}

func (c *RemoteCacheConn) handleHello(env domain.RCEnvelope) {
	var hello domain.Hello
	if err := env.DecodeBody(&hello); err != nil {
		_ = c.replyError(env, "invalid hello payload")
		return
	}
	if hello.CacheID == "" {
		_ = c.replyError(env, "hello: empty cacheID")
		return
	}
	c.stateMu.Lock()
	if c.welcomed {
		c.stateMu.Unlock()
		_ = c.replyError(env, "already welcomed")
		return
	}
	c.cacheID = hello.CacheID
	c.generation = hello.Generation
	c.period = hello.Period
	c.settled = hello.Settled
	c.welcomed = true
	c.stateMu.Unlock()

	// Welcome must precede every service request on this connection.
	if err := c.reply(env, domain.RCTypeWelcome, domain.Welcome{}); err != nil {
		return
	}
	if hello.Settled {
		// The engine reconnected already settled for this period: it is idle,
		// so the service can export without waiting for another settle.
		c.startReadThrough()
	}
}

func (c *RemoteCacheConn) handleOpen(env domain.RCEnvelope) {
	var open domain.Open
	if err := env.DecodeBody(&open); err != nil {
		_ = c.replyError(env, "invalid open payload")
		return
	}
	c.stateMu.Lock()
	if open.Period > c.period {
		c.period = open.Period
	}
	c.settled = false
	c.stateMu.Unlock()
	_ = c.reply(env, domain.RCTypeOpened, domain.Opened{})
}

func (c *RemoteCacheConn) handleClaims(env domain.RCEnvelope) {
	var claims domain.Claims
	if err := env.DecodeBody(&claims); err != nil {
		_ = c.replyError(env, "invalid claims payload")
		return
	}
	_ = c.reply(env, domain.RCTypeClaimed, domain.Claimed{})
}

func (c *RemoteCacheConn) handleSettle(env domain.RCEnvelope) {
	var settle domain.Settle
	if err := env.DecodeBody(&settle); err != nil {
		_ = c.replyError(env, "invalid settle payload")
		return
	}
	c.stateMu.Lock()
	if c.period != 0 && settle.Period < c.period {
		c.stateMu.Unlock()
		_ = c.replyError(env, domain.RCStaleSettle)
		return
	}
	if settle.Period > c.period {
		c.period = settle.Period
	}
	c.settled = true
	c.lastSettled = settle.Period
	c.stateMu.Unlock()

	if err := c.reply(env, domain.RCTypeSettled, domain.Settled{}); err != nil {
		return
	}
	c.startReadThrough()
}

func (c *RemoteCacheConn) startReadThrough() {
	if c.readOnly {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c.readThrough(ctx)
	}()
}

// readThrough exports the roots learned from telemetry for this engine cache,
// stores each bundle, and asks the engine to upload the layer blobs.
func (c *RemoteCacheConn) readThrough(ctx context.Context) {
	c.stateMu.Lock()
	cacheID := c.cacheID
	c.stateMu.Unlock()
	if cacheID == "" {
		return
	}
	roots := c.svc.cacheRoots(cacheID)
	for len(roots) > 0 {
		batch := roots
		if len(batch) > exportRootsPerRequest {
			batch = batch[:exportRootsPerRequest]
		}
		if err := c.exportBatch(ctx, batch); err != nil {
			c.logger.WithError(err).WithField("namespace", c.namespace).Debug("remote cache: export failed")
			return
		}
		numbers := make([]uint64, len(batch))
		for i := range batch {
			numbers[i] = batch[i].Number
		}
		c.svc.forgetCacheRoots(cacheID, numbers)
		roots = c.svc.cacheRoots(cacheID)
	}
}

// exportBatch runs one export → store → upload chain for roots.
func (c *RemoteCacheConn) exportBatch(ctx context.Context, roots []CacheRootRef) error {
	if err := c.svc.exportSem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.svc.exportSem.Release(1)

	reqCtx, cancel := context.WithTimeout(ctx, remoteCacheRequestTimeout)
	defer cancel()
	numbers := make([]uint64, len(roots))
	recipes := make([]string, len(roots))
	for i := range roots {
		numbers[i] = roots[i].Number
		recipes[i] = roots[i].Recipe
	}
	reply, err := c.request(reqCtx, domain.RCTypeExport, domain.Export{Roots: numbers})
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	var exported domain.Exported
	if err := reply.DecodeBody(&exported); err != nil {
		return fmt.Errorf("exported: %w", err)
	}
	if len(exported.Bundle) == 0 {
		c.logger.WithField("namespace", c.namespace).Debug("remote cache: export returned no bundle")
		return nil
	}
	if _, err := c.svc.storeBundle(ctx, c.namespace, recipes, exported.Bundle); err != nil {
		return fmt.Errorf("store bundle: %w", err)
	}
	upload, err := c.svc.handleUpload(ctx, c.namespace, exported.ExportID, exported.Bundle)
	if err != nil {
		return fmt.Errorf("presign upload: %w", err)
	}
	if len(upload.URLs) == 0 && len(upload.Packs) == 0 {
		return nil
	}
	uploadCtx, uploadCancel := context.WithTimeout(ctx, remoteCacheRequestTimeout)
	defer uploadCancel()
	uploadReply, err := c.request(uploadCtx, domain.RCTypeUpload, *upload)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	var uploaded domain.Uploaded
	_ = uploadReply.DecodeBody(&uploaded)
	c.logger.WithFields(logrus.Fields{
		"namespace": c.namespace,
		"blobs":     len(upload.URLs),
		"done":      len(uploaded.Done),
		"failed":    len(uploaded.Failed),
	}).Debug("remote cache: export stored")
	return nil
}

// handleLookup answers one batch of cache misses. A hit is reported as
// "merging" and delivered by a subsequent merge that names this lookup.
func (c *RemoteCacheConn) handleLookup(ctx context.Context, env domain.RCEnvelope) {
	var lookup domain.Lookup
	if err := env.DecodeBody(&lookup); err != nil {
		_ = c.replyError(env, "invalid lookup payload")
		return
	}
	if len(lookup.Misses) > domain.RCMaxLookupMisses {
		lookup.Misses = lookup.Misses[:domain.RCMaxLookupMisses]
	}
	items := make([]domain.LookedUpMiss, len(lookup.Misses))
	type mergeRef struct {
		id     uint64
		recipe string
		bundle []byte
	}
	var merges []mergeRef
	for i := range lookup.Misses {
		miss := &lookup.Misses[i]
		items[i] = domain.LookedUpMiss{Status: domain.RCLookupNotStored}
		if c.readOnly || miss.Recipe == "" {
			continue
		}
		if c.hasMerged(miss.Recipe) {
			items[i].Status = domain.RCLookupInFlight
			continue
		}
		bundle, ok := c.svc.lookupBundle(ctx, c.namespace, miss)
		if !ok {
			continue
		}
		id := c.nextID()
		items[i] = domain.LookedUpMiss{Status: domain.RCLookupMerging, Merge: id, Parts: domain.RCLookupPartsComplete}
		merges = append(merges, mergeRef{id: id, recipe: miss.Recipe, bundle: bundle})
	}
	if err := c.reply(env, domain.RCTypeLookedUp, domain.LookedUp{Misses: items}); err != nil {
		return
	}
	for _, merge := range merges {
		c.markMerged(merge.recipe)
		if err := c.startRequest(merge.id, domain.RCTypeMerge, domain.Merge{Bundle: merge.bundle, Lookup: env.ID}); err != nil {
			c.logger.WithError(err).Debug("remote cache: merge send failed")
			return
		}
	}
}

func (c *RemoteCacheConn) handleRenew(ctx context.Context, env domain.RCEnvelope) {
	var renew domain.Renew
	if err := env.DecodeBody(&renew); err != nil {
		_ = c.replyError(env, "invalid renew payload")
		return
	}
	renewed, err := c.svc.handleRenew(ctx, c.namespace, &renew)
	if err != nil {
		_ = c.replyError(env, err.Error())
		return
	}
	_ = c.reply(env, domain.RCTypeRenewed, renewed)
}

// request sends a service request and waits for its reply.
func (c *RemoteCacheConn) request(ctx context.Context, typ string, body any) (domain.RCEnvelope, error) {
	return c.requestAs(ctx, c.nextID(), typ, body)
}

// requestAs is request with a caller-chosen ID.
func (c *RemoteCacheConn) requestAs(ctx context.Context, id uint64, typ string, body any) (domain.RCEnvelope, error) {
	ch, err := c.register(id)
	if err != nil {
		return domain.RCEnvelope{}, err
	}
	defer c.unregister(id)
	if err := c.write(id, 0, typ, body); err != nil {
		return domain.RCEnvelope{}, err
	}
	select {
	case reply := <-ch:
		if reply.Type == domain.RCTypeError {
			var body domain.RemoteCacheError
			_ = reply.DecodeBody(&body)
			return reply, fmt.Errorf("%s: %s", typ, body.Message)
		}
		return reply, nil
	case <-ctx.Done():
		return domain.RCEnvelope{}, ctx.Err()
	case <-c.done:
		return domain.RCEnvelope{}, errRemoteCacheConnClosed
	}
}

// startRequest sends a request whose reply nobody waits for. The reply is still
// matched and dropped, so it never reaches the unknown-request path.
func (c *RemoteCacheConn) startRequest(id uint64, typ string, body any) error {
	if _, err := c.register(id); err != nil {
		return err
	}
	return c.write(id, 0, typ, body)
}

// reply answers a request.
func (c *RemoteCacheConn) reply(to domain.RCEnvelope, typ string, body any) error {
	return c.write(c.nextID(), to.ID, typ, body)
}

// replyError answers a request with an error reply.
func (c *RemoteCacheConn) replyError(to domain.RCEnvelope, message string) error {
	env, err := domain.NewRCEnvelope(c.nextID(), to.ID, domain.RCTypeError, domain.RemoteCacheError{Message: message})
	if err != nil {
		return err
	}
	return c.sendFrame(env)
}

func (c *RemoteCacheConn) write(id, re uint64, typ string, body any) error {
	env, err := domain.NewRCEnvelope(id, re, typ, body)
	if err != nil {
		return err
	}
	return c.sendFrame(env)
}

func (c *RemoteCacheConn) sendFrame(env domain.RCEnvelope) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errRemoteCacheConnClosed
	}
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	return c.send(data)
}

func (c *RemoteCacheConn) nextID() uint64 {
	return c.lastID.Add(1)
}

func (c *RemoteCacheConn) register(id uint64) (chan domain.RCEnvelope, error) {
	ch := make(chan domain.RCEnvelope, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errRemoteCacheConnClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	return ch, nil
}

func (c *RemoteCacheConn) unregister(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *RemoteCacheConn) hasMerged(recipe string) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.merged[recipe]
}

func (c *RemoteCacheConn) markMerged(recipe string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.merged == nil {
		c.merged = make(map[string]bool)
	}
	c.merged[recipe] = true
}
