package kv

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// Replicator is the node-to-node transport. Replication rides the same HTTP
// surface as client traffic, just under /internal, which keeps the cluster
// deployable as plain containers with no extra ports or protocols.
//
// Each peer gets its own client, and so its own circuit breaker. A single
// shared breaker would be actively harmful here: one dead replica would open
// it and start refusing writes to the healthy ones too, turning the loss of a
// replica into the loss of the quorum.
type Replicator struct {
	timeout time.Duration

	mu      sync.RWMutex
	clients map[string]*httpx.Client
}

// NewReplicator returns a Replicator whose calls time out after rpcTimeout.
func NewReplicator(rpcTimeout time.Duration) *Replicator {
	return &Replicator{timeout: rpcTimeout, clients: make(map[string]*httpx.Client)}
}

func (r *Replicator) clientFor(peer string) *httpx.Client {
	r.mu.RLock()
	client, ok := r.clients[peer]
	r.mu.RUnlock()
	if ok {
		return client
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if client, ok := r.clients[peer]; ok {
		return client
	}

	client = httpx.NewClient(httpx.ClientConfig{
		// Labelled per peer so metrics and breaker state name the specific
		// replica that is struggling. Peers are a small fixed set, so this
		// cannot grow unbounded.
		Dependency:     "kv-peer:" + hostOf(peer),
		ConnectTimeout: 500 * time.Millisecond,
		RequestTimeout: r.timeout,
		// Replication is already retried at a higher level by the quorum
		// logic, which can succeed via a different peer. Retrying here as well
		// would multiply the load on a node that is already failing.
		MaxAttempts: 1,
	})
	r.clients[peer] = client
	return client
}

// Push sends a versioned entry to a peer and waits for it to be applied.
func (r *Replicator) Push(ctx context.Context, peer string, e Entry) error {
	err := r.clientFor(peer).PutJSON(ctx, peer+"/internal/kv", e, nil)
	obs.ObserveReplication("push", err)
	return err
}

// Fetch reads a peer's local copy of key.
//
// A peer that simply does not hold the key answers 404, which is a valid vote
// toward a read quorum rather than a failure, so it comes back as
// (zero, false, nil).
func (r *Replicator) Fetch(ctx context.Context, peer, key string) (Entry, bool, error) {
	var e Entry
	err := r.clientFor(peer).GetJSON(ctx, peer+"/internal/kv?key="+url.QueryEscape(key), &e)
	if err == nil {
		obs.ObserveReplication("fetch", nil)
		return e, true, nil
	}

	if httpx.IsStatus(err, http.StatusNotFound) {
		obs.ObserveReplication("fetch", nil)
		return Entry{}, false, nil
	}
	obs.ObserveReplication("fetch", err)
	return Entry{}, false, err
}

// Scan reads a peer's local view of a prefix. Used to build the union that
// makes a quorum scan complete.
func (r *Replicator) Scan(ctx context.Context, peer, prefix string, limit int) ([]Entry, error) {
	var entries []Entry
	url := fmt.Sprintf("%s/internal/kv/scan?prefix=%s&limit=%d",
		peer, url.QueryEscape(prefix), limit)

	if err := r.clientFor(peer).GetJSON(ctx, url, &entries); err != nil {
		obs.ObserveReplication("scan", err)
		return nil, err
	}
	obs.ObserveReplication("scan", nil)
	return entries, nil
}

// hostOf reduces a peer URL to a label-safe host, so metric series are named
// by replica rather than by full URL.
func hostOf(peer string) string {
	parsed, err := url.Parse(peer)
	if err != nil || parsed.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(peer, "http://"), "https://")
	}
	return parsed.Host
}
