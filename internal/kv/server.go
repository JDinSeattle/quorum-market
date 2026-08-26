package kv

import (
	"net/http"
	"strconv"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/busywait"
	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// Server exposes a KV node over HTTP.
type Server struct {
	svc *Service
	txn *TxnManager

	delay busywait.Config
	// txnBusyWait makes the transaction endpoints burn CPU like a service
	// handler. It defaults off: DB nodes sit outside the Auto Scaling Groups,
	// so burning CPU there only starves the store without moving any scaling
	// metric. WRITE_DELAY_MS / READ_DELAY_MS already model storage cost.
	txnBusyWait bool
}

// NewServer returns a Server for one node.
func NewServer(svc *Service, txn *TxnManager, delay busywait.Config, txnBusyWait bool) *Server {
	return &Server{svc: svc, txn: txn, delay: delay, txnBusyWait: txnBusyWait}
}

type writeRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// TTLMs expires the entry after a fixed lifetime. Zero stores it forever.
	// Callers that write bounded-lifetime data — idempotency records, session
	// state — set this so the store does not grow without limit.
	TTLMs int64 `json:"ttlMs,omitempty"`
}

type writeResponse struct {
	Key       string `json:"key"`
	Version   int64  `json:"version"`
	Origin    string `json:"origin"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
}

type txnRequest struct {
	TransactionID string `json:"transaction_id"`
}

// Routes builds the node's HTTP surface.
//
// /kv is the client API, /internal/kv is the replication API, and /db holds
// the simulated transaction endpoints. Splitting client from internal traffic
// keeps replication off the load balancer's path patterns.
func (s *Server) Routes() http.Handler {
	rt := httpx.NewRouter()

	rt.Probe("GET /health")

	// Client API
	rt.Handle("GET /kv", s.handleRead)
	rt.Handle("PUT /kv", s.handleWrite)
	rt.Handle("POST /kv", s.handleWrite)
	rt.Handle("GET /kv/local", s.handleLocalRead)
	rt.Handle("GET /kv/scan", s.handleScan)
	rt.Handle("GET /kv/stats", s.handleStats)

	// Replication API
	rt.Handle("PUT /internal/kv", s.handleReplicate)
	rt.Handle("GET /internal/kv", s.handleInternalRead)
	rt.Handle("GET /internal/kv/scan", s.handleInternalScan)

	// Simulated transaction API
	rt.Handle("POST /db/begin_transaction", s.handleBegin)
	rt.Handle("POST /db/end_transaction", s.handleEnd)
	rt.Handle("POST /db/abort_transaction", s.handleAbort)

	return rt.Build(httpx.DefaultMaxInFlight())
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) error {
	var req writeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.TTLMs < 0 {
		return httpx.Errorf(http.StatusBadRequest, "ttlMs must not be negative")
	}

	entry, err := s.svc.Write(r.Context(), req.Key, req.Value, time.Duration(req.TTLMs)*time.Millisecond)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, writeResponse{
		Key: entry.Key, Version: entry.Version, Origin: entry.Origin, ExpiresAt: entry.ExpiresAt,
	})
	return nil
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) error {
	key := r.URL.Query().Get("key")
	entry, found, err := s.svc.Read(r.Context(), key)
	if err != nil {
		return err
	}
	if !found {
		return httpx.Errorf(http.StatusNotFound, "key %q not found", key)
	}
	httpx.JSON(w, http.StatusOK, entry)
	return nil
}

func (s *Server) handleLocalRead(w http.ResponseWriter, r *http.Request) error {
	key := r.URL.Query().Get("key")
	entry, found := s.svc.LocalRead(key)
	if !found {
		return httpx.Errorf(http.StatusNotFound, "key %q not found on %s", key, s.svc.Config().NodeID)
	}
	httpx.JSON(w, http.StatusOK, entry)
	return nil
}

func (s *Server) handleReplicate(w http.ResponseWriter, r *http.Request) error {
	var entry Entry
	if err := httpx.DecodeJSON(r, &entry); err != nil {
		return err
	}
	if err := s.svc.ApplyReplication(r.Context(), entry); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleInternalRead serves a peer's quorum read. It never fans out further,
// which is what stops a quorum read from recursing across the cluster.
func (s *Server) handleInternalRead(w http.ResponseWriter, r *http.Request) error {
	key := r.URL.Query().Get("key")
	entry, found := s.svc.LocalRead(key)
	if !found {
		return httpx.Errorf(http.StatusNotFound, "key %q not found", key)
	}
	httpx.JSON(w, http.StatusOK, entry)
	return nil
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) error {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		// An unbounded scan of the whole keyspace is never what a caller
		// wants and always what takes the node down.
		return httpx.Errorf(http.StatusBadRequest, "prefix is required")
	}

	entries, err := s.svc.Scan(r.Context(), prefix, scanLimit(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, entries)
	return nil
}

// handleInternalScan serves one replica's own view, with no fan-out, which is
// what stops a quorum scan recursing across the cluster.
func (s *Server) handleInternalScan(w http.ResponseWriter, r *http.Request) error {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		return httpx.Errorf(http.StatusBadRequest, "prefix is required")
	}
	httpx.JSON(w, http.StatusOK, s.svc.LocalScan(prefix, scanLimit(r)))
	return nil
}

func scanLimit(r *http.Request) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return 200
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) error {
	cfg := s.svc.Config()
	httpx.JSON(w, http.StatusOK, map[string]any{
		"node_id":                cfg.NodeID,
		"mode":                   cfg.Mode,
		"role":                   cfg.Role,
		"cluster_size":           cfg.ClusterSize(),
		"write_quorum":           cfg.WriteQuorum,
		"read_quorum":            cfg.ReadQuorum,
		"strongly_consistent":    cfg.StronglyConsistent(),
		"sequential_replication": cfg.Sequential,
		"read_repair":            cfg.ReadRepair,
		"keys":                   s.svc.Keys(),
		"transactions":           s.txn.Stats(),
	})
	return nil
}

func (s *Server) handleBegin(w http.ResponseWriter, r *http.Request) error {
	s.maybeBurn()
	httpx.JSON(w, http.StatusOK, s.txn.Begin())
	return nil
}

func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) error {
	return s.resolveTxn(w, r, s.txn.Commit)
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) error {
	return s.resolveTxn(w, r, s.txn.Abort)
}

func (s *Server) resolveTxn(w http.ResponseWriter, r *http.Request, resolve func(string) (*Txn, error)) error {
	var req txnRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	s.maybeBurn()
	txn, err := resolve(req.TransactionID)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, txn)
	return nil
}

func (s *Server) maybeBurn() {
	if s.txnBusyWait {
		s.delay.Simulate()
	}
}
