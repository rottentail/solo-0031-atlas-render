package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/raster"
	"atlas-render/internal/scene"
	"sort"
	"time"
)

// BatchMember is the query view of one member job of a batch.
type BatchMember struct {
	JobID      string `json:"jobId"`
	Scale      int    `json:"scale"`
	State      string `json:"state"`
	Attempt    int    `json:"attempt"`
	Error      string `json:"error,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
}

// BatchView is the queryable output set returned over HTTP.
type BatchView struct {
	ID         string        `json:"id"`
	SnapshotID string        `json:"snapshotId"`
	State      string        `json:"state"`
	Scales     []int         `json:"scales"`
	Members    []BatchMember `json:"members"`
	CreatedAt  time.Time     `json:"createdAt"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

// pendingJobs counts jobs that still occupy a queue slot.
func pendingJobs(st *model.State) int {
	pending := 0
	for _, j := range st.Jobs {
		if j.State == "queued" || j.State == "running" {
			pending++
		}
	}
	return pending
}

// SubmitBatch accepts a whole multi-scale output set in one transaction.
// Either all member jobs and the batch record commit together, or the state
// is left untouched. Replaying the same idempotency key returns the original
// batch; the same key with different request semantics is rejected.
func (s *Service) SubmitBatch(snapshotID string, scales []int, key string) (model.Batch, error) {
	var out model.Batch
	if !scene.ValidID(key) {
		return out, model.Invalid("idempotency key required (1..80 identifier characters)")
	}
	normalized, e := normalizeScales(scales)
	if e != nil {
		return out, e
	}
	fingerprint := model.CanonicalDigest(struct {
		SnapshotID string
		Scales     []int
	}{snapshotID, normalized})
	e = s.Store.Update(func(st *model.State) error {
		if r, ok := st.Receipts[key]; ok {
			if r.Fingerprint != fingerprint || r.BatchID == "" {
				return model.Conflict("idempotency key used for different request")
			}
			out = st.Batches[r.BatchID]
			return nil
		}
		snap, ok := st.Snapshots[snapshotID]
		if !ok {
			return model.Missing("snapshot not found")
		}
		if snap.State != "active" {
			return model.Conflict("snapshot is revoked")
		}
		// Compile every member before writing anything: an unaffordable member
		// rejects the whole set, so an over-budget request never leaves jobs.
		for _, scale := range normalized {
			if _, e := raster.Compile(snap, scale); e != nil {
				return e
			}
		}
		if pendingJobs(st)+len(normalized) > 64 {
			return model.Busy("render queue is full")
		}
		now := time.Now().UTC()
		batchID := model.ID("batch", st.Sequence+1)
		out = model.Batch{ID: batchID, SnapshotID: snapshotID, State: model.BatchActive, Scales: normalized, CreatedAt: now, UpdatedAt: now}
		for i, scale := range normalized {
			j := model.Job{
				ID:         model.ID("job", st.Sequence+2+i),
				SnapshotID: snapshotID,
				State:      "queued",
				Scale:      scale,
				Attempt:    1,
				BatchID:    batchID,
				CreatedAt:  now,
				UpdatedAt:  now,
				Payload:    snap,
			}
			st.Jobs[j.ID] = j
			out.JobIDs = append(out.JobIDs, j.ID)
		}
		st.Batches[batchID] = out
		st.Receipts[key] = model.Receipt{Key: key, Fingerprint: fingerprint, BatchID: batchID}
		st.Emit("batch.queued", batchID)
		for _, jid := range out.JobIDs {
			st.Emit("job.queued", jid)
		}
		return nil
	})
	return out, e
}

// normalizeScales enforces a non-empty, de-duplicated set of supported scales
// in ascending order.
func normalizeScales(scales []int) ([]int, error) {
	if len(scales) == 0 {
		return nil, model.Invalid("at least one scale is required")
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(scales))
	for _, scale := range scales {
		if scale != 1 && scale != 2 {
			return nil, model.Invalid("scale must be one or two")
		}
		if seen[scale] {
			return nil, model.Invalid("scales must not repeat")
		}
		seen[scale] = true
		out = append(out, scale)
	}
	sort.Ints(out)
	return out, nil
}

// GetBatch returns the committed batch and its current member states.
func (s *Service) GetBatch(id string) (BatchView, error) {
	st := s.Store.Read()
	b, ok := st.Batches[id]
	if !ok {
		return BatchView{}, model.Missing("batch not found")
	}
	return batchView(st, b), nil
}

func batchView(st model.State, b model.Batch) BatchView {
	view := BatchView{ID: b.ID, SnapshotID: b.SnapshotID, State: b.State, Scales: append([]int(nil), b.Scales...), CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
	for _, jid := range b.JobIDs {
		j := st.Jobs[jid]
		view.Members = append(view.Members, BatchMember{JobID: jid, Scale: j.Scale, State: j.State, Attempt: j.Attempt, Error: j.Error, ArtifactID: j.ArtifactID})
	}
	return view
}

// CancelBatch cancels every still-cancellable member in one transaction.
// Already terminal members keep their outcomes, which keeps mixed results
// explainable.
func (s *Service) CancelBatch(id string) (model.Batch, error) {
	var out model.Batch
	e := s.Store.Update(func(st *model.State) error {
		b, ok := st.Batches[id]
		if !ok {
			return model.Missing("batch not found")
		}
		cancellable := 0
		for _, jid := range b.JobIDs {
			if st.Jobs[jid].State == "queued" || st.Jobs[jid].State == "running" {
				cancellable++
			}
		}
		if cancellable == 0 {
			return model.Conflict("batch has already finished")
		}
		now := time.Now().UTC()
		for _, jid := range b.JobIDs {
			j := st.Jobs[jid]
			if j.State != "queued" && j.State != "running" {
				continue
			}
			j.State = "cancelled"
			j.UpdatedAt = now
			st.Jobs[jid] = j
			st.Emit("job.cancelled", jid)
		}
		st.ReconcileBatches()
		st.Emit("batch.cancelled", id)
		out = st.Batches[id]
		return nil
	})
	return out, e
}

// RetryBatch re-queues every failed or cancelled member together. Successful
// members are never retried. Capacity is checked against the whole set first,
// so a full queue cannot partially reopen the batch.
func (s *Service) RetryBatch(id string) (model.Batch, error) {
	var out model.Batch
	e := s.Store.Update(func(st *model.State) error {
		b, ok := st.Batches[id]
		if !ok {
			return model.Missing("batch not found")
		}
		retryable := 0
		for _, jid := range b.JobIDs {
			if st.Jobs[jid].State == "failed" || st.Jobs[jid].State == "cancelled" {
				retryable++
			}
		}
		if retryable == 0 {
			return model.Conflict("batch has no failed or cancelled members")
		}
		if pendingJobs(st)+retryable > 64 {
			return model.Busy("render queue is full")
		}
		now := time.Now().UTC()
		for _, jid := range b.JobIDs {
			j := st.Jobs[jid]
			if j.State != "failed" && j.State != "cancelled" {
				continue
			}
			j.State = "queued"
			j.Error = ""
			j.Attempt++
			j.UpdatedAt = now
			st.Jobs[jid] = j
			st.Emit("job.retried", jid)
		}
		st.ReconcileBatches()
		st.Emit("batch.retried", id)
		out = st.Batches[id]
		return nil
	})
	return out, e
}
