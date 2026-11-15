package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"path/filepath"
	"testing"
)

func newTestService(t *testing.T) (*Service, model.Snapshot) {
	t.Helper()
	store, e := persist.Open(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatalf("open store: %v", e)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := &Service{Store: store}
	doc := model.Document{Width: 8, Height: 8, Background: "#ffffff", Layers: []model.Layer{{
		ID: "r1", Kind: "rectangle", Width: 4, Height: 4, Opacity: 1,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
		Fill:      model.Paint{Kind: "solid", Color: "#102030"},
	}}}
	var snap model.Snapshot
	if e := store.Update(func(st *model.State) error {
		st.Scenes["sc1"] = model.Scene{ID: "sc1", Name: "n", Revision: 1, State: "active", Document: doc}
		snap = model.Snapshot{ID: "snap-00000001", SceneID: "sc1", Revision: 1, State: "active", Document: doc, Assets: map[string]model.Asset{}}
		snap.Digest = model.SnapshotDigest(snap)
		st.Snapshots[snap.ID] = snap
		return nil
	}); e != nil {
		t.Fatalf("seed state: %v", e)
	}
	return svc, snap
}

func TestSubmitBatchIsAtomicAndIdempotent(t *testing.T) {
	svc, snap := newTestService(t)
	batch, e := svc.SubmitBatch(snap.ID, []int{2, 1}, "key-batch")
	if e != nil {
		t.Fatalf("submit batch: %v", e)
	}
	if batch.State != model.BatchActive || len(batch.JobIDs) != 2 {
		t.Fatalf("unexpected batch: %+v", batch)
	}
	// Replay returns the same set and never creates extra member jobs.
	replay, e := svc.SubmitBatch(snap.ID, []int{1, 2}, "key-batch")
	if e != nil {
		t.Fatalf("replay: %v", e)
	}
	if replay.ID != batch.ID || len(replay.JobIDs) != 2 {
		t.Fatalf("replay must return the identical batch, got %+v", replay)
	}
	st := svc.Store.Read()
	if len(st.Jobs) != 2 || len(st.Batches) != 1 {
		t.Fatalf("replay duplicated work: jobs=%d batches=%d", len(st.Jobs), len(st.Batches))
	}
	// Same key, different semantics conflicts without writing anything.
	if _, e := svc.SubmitBatch(snap.ID, []int{1}, "key-batch"); e == nil {
		t.Fatal("changed request under same key must conflict")
	}
	if _, e := svc.Submit(snap.ID, 1, "key-batch"); e == nil {
		t.Fatal("single-job replay of a batch key must conflict")
	}
	st = svc.Store.Read()
	if len(st.Jobs) != 2 {
		t.Fatalf("conflicting replays must not create jobs, got %d", len(st.Jobs))
	}
}

func TestSubmitBatchRejectsInvalidSetsBeforeWriting(t *testing.T) {
	svc, snap := newTestService(t)
	cases := map[string][]int{
		"empty":     {},
		"duplicate": {1, 1},
		"bad scale": {3},
		"mixed bad": {1, 9},
	}
	for name, scales := range cases {
		if _, e := svc.SubmitBatch(snap.ID, scales, "key-"+name); e == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
	st := svc.Store.Read()
	if len(st.Jobs) != 0 || len(st.Batches) != 0 || len(st.Receipts) != 0 {
		t.Fatalf("rejected batch left state behind: jobs=%d batches=%d receipts=%d", len(st.Jobs), len(st.Batches), len(st.Receipts))
	}
}

func TestCancelAndRetryBatchKeepSetConsistent(t *testing.T) {
	svc, snap := newTestService(t)
	batch, e := svc.SubmitBatch(snap.ID, []int{1, 2}, "key-mix")
	if e != nil {
		t.Fatalf("submit: %v", e)
	}
	// Settle member 1 as succeeded directly through a committed transaction.
	if e := svc.Store.Update(func(st *model.State) error {
		j := st.Jobs[batch.JobIDs[0]]
		j.State = "succeeded"
		j.ArtifactID = "png-x"
		png := []byte{1, 2, 3, 4}
		st.Jobs[j.ID] = j
		st.Artifacts["png-x"] = model.Artifact{ID: "png-x", JobID: j.ID, State: "live", Width: 8, Height: 8, PNG: png, Digest: model.Digest(png)}
		st.ReconcileBatches()
		return nil
	}); e != nil {
		t.Fatalf("seed success: %v", e)
	}
	// Cancel the set: only the unfinished member moves; success is preserved.
	cancelled, e := svc.CancelBatch(batch.ID)
	if e != nil {
		t.Fatalf("cancel: %v", e)
	}
	if cancelled.State != model.BatchSettled {
		t.Fatalf("mixed set should settle, got %s", cancelled.State)
	}
	view, _ := svc.GetBatch(batch.ID)
	states := map[int]string{}
	for _, m := range view.Members {
		states[m.Scale] = m.State
	}
	if states[1] != "succeeded" || states[2] != "cancelled" {
		t.Fatalf("unexpected mixed outcome: %+v", states)
	}
	if _, e := svc.CancelBatch(batch.ID); e == nil {
		t.Fatal("cancelling a finished batch must conflict")
	}
	// Retry reopens only the cancelled member and bumps its attempt.
	reopened, e := svc.RetryBatch(batch.ID)
	if e != nil {
		t.Fatalf("retry: %v", e)
	}
	if reopened.State != model.BatchActive {
		t.Fatalf("retry should reopen the set, got %s", reopened.State)
	}
	view, _ = svc.GetBatch(batch.ID)
	for _, m := range view.Members {
		switch m.Scale {
		case 1:
			if m.State != "succeeded" {
				t.Fatalf("successful member must not be retried: %+v", m)
			}
		case 2:
			if m.State != "queued" || m.Attempt != 2 {
				t.Fatalf("cancelled member should be queued attempt 2: %+v", m)
			}
		}
	}
}

func TestRetryBatchWithoutRetryableMembersConflicts(t *testing.T) {
	svc, snap := newTestService(t)
	batch, _ := svc.SubmitBatch(snap.ID, []int{1}, "key-one")
	if e := svc.Store.Update(func(st *model.State) error {
		png := []byte{5, 6, 7, 8}
		for _, jid := range batch.JobIDs {
			j := st.Jobs[jid]
			j.State = "succeeded"
			j.ArtifactID = "png-y"
			st.Jobs[jid] = j
			st.Artifacts["png-y"] = model.Artifact{ID: "png-y", JobID: jid, State: "live", Width: 8, Height: 8, PNG: png, Digest: model.Digest(png)}
		}
		st.ReconcileBatches()
		return nil
	}); e != nil {
		t.Fatalf("seed: %v", e)
	}
	if _, e := svc.RetryBatch(batch.ID); e == nil {
		t.Fatal("retrying a fully succeeded batch must conflict")
	}
}
