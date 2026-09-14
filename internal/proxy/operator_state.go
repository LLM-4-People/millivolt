package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
)

// Runtime policy already changed before durable I/O. It cannot safely be
// rolled back: resuming may have admitted requests. Keep persistence failures
// distinct from rejected edits so every operator control reports live state.
type operatorPersistenceError struct{ err error }

func (e *operatorPersistenceError) Error() string { return e.err.Error() }
func (e *operatorPersistenceError) Unwrap() error { return e.err }

func writeOperatorState(w http.ResponseWriter, state map[string]any, persistErr error) {
	if persistErr != nil {
		state["warning"] = "State applied in memory, but could not be saved for restart."
	}
	enc, err := json.Marshal(state)
	if err != nil {
		log.Printf("operator state: encode failed: %v", err)
		adminjson.WriteError(w, http.StatusInternalServerError, "could not encode operator state")
		return
	}
	w.Write(enc)
}

// persistOperatorDoc marshals one operator-state snapshot and stores it via
// the feature's Save* callback. It owns the shared write half of every
// operator persist: the marshal, the bounded store context, and the
// "<feature>: persist" failure log. The callback and its lock ownership stay
// at the call site.
func (s *Server) persistOperatorDoc(prefix string, doc any, save func(context.Context, []byte) error) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		log.Printf("%s: persist: %v", prefix, err)
		return err
	}
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if err := save(ctx, raw); err != nil {
		log.Printf("%s: persist: %v", prefix, err)
		return err
	}
	return nil
}

// persistStableGen owns the generation-guarded retry loop shared by the
// operator-state persisters. write stores the snapshot it is handed; refetch,
// called under mu, returns the latest snapshot paired with its generation.
// A write whose generation has moved is superseded - the loop refetches and
// writes again. Only a write that lands on an unchanged generation returns;
// its error, if any, becomes the operatorPersistenceError surfaced to the
// operator alongside live state.
func persistStableGen[T any](snap T, gen uint64, mu *sync.Mutex, genOf func() uint64, write func(T) error, refetch func() (T, uint64)) error {
	for {
		err := write(snap)
		mu.Lock()
		if genOf() == gen {
			mu.Unlock()
			if err != nil {
				return &operatorPersistenceError{err}
			}
			return nil
		}
		snap, gen = refetch()
		mu.Unlock()
	}
}

// operatorStateGet gates one operator-state endpoint (method check, JSON
// content type) and serves its GET snapshot. It returns true only for a POST
// the caller must still decode and apply.
func (s *Server) operatorStateGet(w http.ResponseWriter, r *http.Request, snapshot func() map[string]any) bool {
	if !rejectUnlessGetPost(w, r) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		writeOperatorState(w, snapshot(), nil)
		return false
	}
	return true
}

// resetOperatorState owns the clear branch shared by the operator-state
// endpoints: apply the removal, log the operator-visible line, then report
// live state with any persistence warning. The caller decides what "clear"
// means - one id, the whole feature, or one provider's cap.
func (s *Server) resetOperatorState(w http.ResponseWriter, snapshot func() map[string]any, clear func() error, logMsg string, logArgs ...any) {
	persistErr := clear()
	log.Printf(logMsg, logArgs...)
	writeOperatorState(w, snapshot(), persistErr)
}

// writeOperatorEditError maps an edit failure for the pause and debug
// endpoints: a persistence failure still reports live state (the policy is
// already applied), a scope overlap is 409, an unknown id is 404, and
// anything else is a rejected edit 400.
func (s *Server) writeOperatorEditError(w http.ResponseWriter, err, overlap, notFound error, overlapMsg, notFoundMsg string, snapshot func() map[string]any) {
	var persistErr *operatorPersistenceError
	if errors.As(err, &persistErr) {
		writeOperatorState(w, snapshot(), persistErr)
		return
	}
	if errors.Is(err, overlap) {
		adminjson.WriteError(w, http.StatusConflict, overlapMsg)
		return
	}
	if errors.Is(err, notFound) {
		adminjson.WriteError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	adminjson.WriteError(w, http.StatusBadRequest, err.Error())
}

// rfc3339OrNil renders an operator-state timestamp: RFC 3339 UTC, or nil for
// the zero value (the JSON document then omits the field).
func rfc3339OrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
