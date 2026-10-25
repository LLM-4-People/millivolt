package web

import (
	"net/http"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ---------- bootstrap: shared initial-page and live state ----------

// bootstrapPayload embeds the ring snapshot shape (records, in-flight
// pending registry, counters, cursor metadata) and adds every state surface
// a fresh page renders before the slower chart/explorer fetches land.
type bootstrapPayload struct {
	metrics.ObservedSnapshot
	DashboardVersion string        `json:"dashboard_version"`
	KPI              kpiPayload    `json:"kpi"`
	Dash             any           `json:"dash,omitempty"`
	Pause            any           `json:"pause,omitempty"`
	Throttle         any           `json:"throttle,omitempty"`
	Debug            any           `json:"debug,omitempty"`
	Storm            any           `json:"storm,omitempty"`
	Restart          any           `json:"restart,omitempty"`
	Storage          storageSignal `json:"storage"`
}

type storageSignal struct {
	Enabled bool   `json:"enabled"`
	Dropped uint64 `json:"dropped"`
	// TotalsDegraded reports a failed boot scan: the since-inception KPI
	// totals count only rows committed since that scan. The status footer
	// renders it as the totals-degraded footnote and tooltip.
	TotalsDegraded bool `json:"totals_degraded"`
}

// bootstrapLivePayload is the fields=live view: the ring snapshot and storage
// signal without KPI/operator state. Snapshot and describe need the records.
type bootstrapLivePayload struct {
	metrics.ObservedSnapshot
	Storage storageSignal `json:"storage"`
}

// bootstrapStoragePayload is the fields=storage view: three booleans, no ring
// snapshot or dashboard state.
type bootstrapStoragePayload struct {
	Storage storageSignal `json:"storage"`
}

// bootstrapModelPayload is the fields=model_canon view: the effective rules
// without records or dashboard state.
type bootstrapModelPayload struct {
	ModelCanon any `json:"model_canon"`
}

func (a *AggAPI) storageSignal() storageSignal {
	if a.store == nil {
		return storageSignal{}
	}
	return storageSignal{Enabled: true, Dropped: a.store.Dropped(), TotalsDegraded: a.store.TotalsDegraded()}
}

// HandleBootstrap is THE dashboard's live endpoint - one round trip carrying
// everything the page renders outside the slow chart/explorer scans: the
// ring snapshot (full and capped on a fresh boot via SnapshotRequest;
// a ?since=&feed= delta on tick/poll resumes), the global since-inception
// KPI aggregate, the effective dashboard config, and the operator pause/
// limit state when wired. In-memory only - no history scan. The client
// filters the records at render (log scope is client-side), so the payload
// takes no scope parameters. The traffic chart and explorer breakdown keep
// their own endpoints (the slow full-history scans).
func (a *AggAPI) HandleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	// The two cursor keys this surface consumes go through the strict parse:
	// a malformed pair must never drop a feed pin and leave a stale cursor
	// trusted, and a repeated feed or since is ambiguous.
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "feed", "since", "fields"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// fields is a closed, additive view used by in-process probes. The default
	// remains the complete dashboard document.
	fields := q.Get("fields")
	switch fields {
	case "":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, a.bootstrap(a.buf.SnapshotRequest(q, r.Header)))
	case "storage":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, bootstrapStoragePayload{Storage: a.storageSignal()})
	case "model_canon":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, bootstrapModelPayload{ModelCanon: a.ObserveModels(true, nil)})
	case "live":
		w.Header().Set("Cache-Control", "no-store")
		snapshot := a.buf.SnapshotRequest(q, r.Header)
		writeJSON(w, bootstrapLivePayload{
			ObservedSnapshot: metrics.ObserveSnapshot(snapshot, a.ObserveModels(true, snapshot.Records, snapshot.InFlightRecords)),
			Storage:          a.storageSignal(),
		})
	default:
		adminjson.WriteError(w, http.StatusBadRequest, "fields must be storage, live, or model_canon")
	}
}

// bootstrap owns the initial HTML and live endpoint payload alike. A new
// document always passes a full snapshot; endpoint resumes use the
// buffer's existing feed/cursor trust boundary. No durable history scan.
func (a *AggAPI) bootstrap(snapshot metrics.Snapshot) bootstrapPayload {
	observed := metrics.ObserveSnapshot(snapshot, a.ObserveModels(true, snapshot.Records, snapshot.InFlightRecords))
	payload := bootstrapPayload{ObservedSnapshot: observed, KPI: a.kpi(), DashboardVersion: dashboardContentVersion()}
	payload.Storage = a.storageSignal()
	if a.Dash != nil {
		payload.Dash = a.Dash()
	}
	if a.Pause != nil {
		payload.Pause = a.Pause()
	}
	if a.Throttle != nil {
		payload.Throttle = a.Throttle()
	}
	if a.Debug != nil {
		payload.Debug = a.Debug()
	}
	if a.Storm != nil {
		payload.Storm = a.Storm()
	}
	if a.Restart != nil {
		payload.Restart = a.Restart()
	}
	return payload
}
