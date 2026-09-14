package proxy

import (
	"encoding/json"
	"log"
	"net/http"

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
