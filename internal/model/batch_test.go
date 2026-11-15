package model

import "testing"

func job(id, state string) Job { return Job{ID: id, State: state} }

func TestReconcileBatchesDerivesStateFromMembers(t *testing.T) {
	s := NewState()
	s.Batches["batch-00000001"] = Batch{ID: "batch-00000001", State: BatchActive, JobIDs: []string{"a", "b"}}
	cases := []struct {
		name        string
		states      []string
		want        string
		expectEvent bool
	}{
		{"both queued", []string{"queued", "queued"}, BatchActive, false},
		{"one succeeded one running", []string{"succeeded", "running"}, BatchActive, false},
		{"one succeeded one cancelled", []string{"succeeded", "cancelled"}, BatchSettled, true},
		{"both failed", []string{"failed", "failed"}, BatchSettled, true},
		{"both succeeded", []string{"succeeded", "succeeded"}, BatchCompleted, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.Jobs = map[string]Job{"a": job("a", tc.states[0]), "b": job("b", tc.states[1])}
			s.Batches["batch-00000001"] = Batch{ID: "batch-00000001", State: BatchActive, JobIDs: []string{"a", "b"}}
			seqBefore := s.Sequence
			s.ReconcileBatches()
			if got := s.Batches["batch-00000001"].State; got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
			emitted := s.Sequence != seqBefore
			if emitted != tc.expectEvent {
				t.Fatalf("event emitted = %v, want %v", emitted, tc.expectEvent)
			}
		})
	}
}

func TestReconcileReopensRetriedBatch(t *testing.T) {
	s := NewState()
	s.Batches["batch-00000001"] = Batch{ID: "batch-00000001", State: BatchSettled, JobIDs: []string{"a", "b"}}
	s.Jobs = map[string]Job{
		"a": job("a", "succeeded"),
		"b": job("b", "queued"), // retried from cancelled
	}
	s.ReconcileBatches()
	if s.Batches["batch-00000001"].State != BatchActive {
		t.Fatalf("retried batch must be active, got %s", s.Batches["batch-00000001"].State)
	}
	if len(s.Events) != 1 || s.Events[0].Kind != "batch.reopened" {
		t.Fatalf("expected batch.reopened event, got %+v", s.Events)
	}
}
