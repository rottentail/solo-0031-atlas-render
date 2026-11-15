package httpapi

import (
	"net/http"
)

func (a API) submitBatch(w http.ResponseWriter, r *http.Request) {
	var b struct {
		SnapshotID string `json:"snapshotId"`
		Scales     []int  `json:"scales"`
	}
	if e := decode(w, r, &b); e != nil {
		fail(w, e)
		return
	}
	batch, e := a.Service.SubmitBatch(b.SnapshotID, b.Scales, r.Header.Get("Idempotency-Key"))
	if e != nil {
		fail(w, e)
		return
	}
	view, e := a.Service.GetBatch(batch.ID)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 202, view)
}

func (a API) getBatch(w http.ResponseWriter, r *http.Request) {
	view, e := a.Service.GetBatch(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, view)
}

func (a API) cancelBatch(w http.ResponseWriter, r *http.Request) {
	batch, e := a.Service.CancelBatch(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	// Best-effort interrupt of in-flight members; queued members are blocked
	// by the worker's state re-check and the committed transaction wins.
	for _, jid := range batch.JobIDs {
		a.Worker.Cancel(jid)
	}
	view, e := a.Service.GetBatch(batch.ID)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, view)
}

func (a API) retryBatch(w http.ResponseWriter, r *http.Request) {
	batch, e := a.Service.RetryBatch(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	view, e := a.Service.GetBatch(batch.ID)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 202, view)
}
