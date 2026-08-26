package kv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JDinSeattle/quorum-market/internal/httpx"
)

// TxnState is the lifecycle state of a simulated transaction.
type TxnState string

const (
	// TxnPending is an open transaction that has not been resolved.
	TxnPending TxnState = "pending"
	// TxnCommitted is a transaction the caller closed successfully.
	TxnCommitted TxnState = "committed"
	// TxnAborted is a transaction the caller gave up on.
	TxnAborted TxnState = "aborted"
)

// Txn is one simulated transaction.
//
// These transactions are a bookkeeping shell, not real ACID. There is no undo
// log and no two-phase commit: aborting does not roll back writes already made
// under the transaction id, because the store has no notion of an uncommitted
// write. What the shell does provide is a real, enforced lifecycle — an id
// must exist and be pending before it can be committed or aborted — which is
// enough to make the checkout flow's transaction boundaries explicit and to
// catch an orchestrator that loses track of its own transaction.
type Txn struct {
	ID        string    `json:"transaction_id"`
	State     TxnState  `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// TxnManager tracks open transactions and reaps abandoned ones.
type TxnManager struct {
	mu   sync.Mutex
	txns map[string]*Txn
	ttl  time.Duration

	committed atomic.Uint64
	aborted   atomic.Uint64
	abandoned atomic.Uint64
}

// NewTxnManager returns a manager that reaps transactions left pending for ttl.
func NewTxnManager(ttl time.Duration) *TxnManager {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &TxnManager{txns: make(map[string]*Txn), ttl: ttl}
}

// Begin opens a transaction and returns it.
func (m *TxnManager) Begin() *Txn {
	t := &Txn{ID: newTxnID(), State: TxnPending, StartedAt: time.Now()}
	m.mu.Lock()
	m.txns[t.ID] = t
	m.mu.Unlock()
	return t
}

// Commit closes a pending transaction successfully.
func (m *TxnManager) Commit(id string) (*Txn, error) { return m.resolve(id, TxnCommitted) }

// Abort closes a pending transaction unsuccessfully.
func (m *TxnManager) Abort(id string) (*Txn, error) { return m.resolve(id, TxnAborted) }

func (m *TxnManager) resolve(id string, state TxnState) (*Txn, error) {
	if id == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "transaction_id is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Resolving removes the entry, so presence in the map *is* pendingness.
	// That keeps the map bounded by the number of checkouts actually in
	// flight, at the cost of not being able to tell a double resolve from a
	// bad id — both are the caller losing track of its own transaction, and
	// both are reported the same way.
	t, ok := m.txns[id]
	if !ok {
		return nil, httpx.Errorf(http.StatusNotFound,
			"transaction %s is not open: it was never begun, has already been resolved, or was reaped as abandoned", id)
	}

	t.State = state
	delete(m.txns, id)

	if state == TxnCommitted {
		m.committed.Add(1)
	} else {
		m.aborted.Add(1)
	}
	return t, nil
}

// Open returns the number of currently pending transactions.
func (m *TxnManager) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.txns)
}

// Stats reports lifetime transaction counters.
func (m *TxnManager) Stats() map[string]any {
	return map[string]any{
		"open":      m.Open(),
		"committed": m.committed.Load(),
		"aborted":   m.aborted.Load(),
		"abandoned": m.abandoned.Load(),
	}
}

// Sweep discards transactions left pending past the TTL. Without this an
// orchestrator that crashes between begin and end leaks an entry per attempt,
// and under sustained load that map is an unbounded memory leak.
func (m *TxnManager) Sweep() int {
	cutoff := time.Now().Add(-m.ttl)

	m.mu.Lock()
	defer m.mu.Unlock()

	reaped := 0
	for id, t := range m.txns {
		if t.StartedAt.Before(cutoff) {
			delete(m.txns, id)
			reaped++
		}
	}
	if reaped > 0 {
		m.abandoned.Add(uint64(reaped))
		slog.Warn("reaped abandoned transactions", "count", reaped, "ttl", m.ttl)
	}
	return reaped
}

// RunSweeper sweeps on a ticker until ctx is cancelled.
func (m *TxnManager) RunSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Sweep()
		}
	}
}

// newTxnID returns a random RFC 4122 version 4 UUID string.
func newTxnID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// timestamp-based id keeps the service usable rather than panicking.
		return fmt.Sprintf("txn-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
