package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const FormatVersion = 2

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}
type Transform struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	ScaleX  float64 `json:"scaleX"`
	ScaleY  float64 `json:"scaleY"`
	Degrees float64 `json:"degrees"`
}
type Paint struct {
	Kind     string `json:"kind"`
	Color    string `json:"color"`
	EndColor string `json:"endColor,omitempty"`
	From     Point  `json:"from"`
	To       Point  `json:"to"`
}
type Layer struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Width     float64   `json:"width"`
	Height    float64   `json:"height"`
	Transform Transform `json:"transform"`
	Opacity   float64   `json:"opacity"`
	Fill      Paint     `json:"fill"`
	Points    []Point   `json:"points,omitempty"`
	AssetID   string    `json:"assetId,omitempty"`
	Clip      *Rect     `json:"clip,omitempty"`
	Children  []Layer   `json:"children,omitempty"`
}
type Document struct {
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Background string  `json:"background"`
	Layers     []Layer `json:"layers"`
}
type Scene struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Revision  int       `json:"revision"`
	State     string    `json:"state"`
	Document  Document  `json:"document"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type Asset struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	State  string `json:"state"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	PNG    []byte `json:"png"`
}
type Snapshot struct {
	ID        string           `json:"id"`
	SceneID   string           `json:"sceneId"`
	Revision  int              `json:"revision"`
	State     string           `json:"state"`
	Document  Document         `json:"document"`
	Assets    map[string]Asset `json:"assets"`
	Digest    string           `json:"digest"`
	CreatedAt time.Time        `json:"createdAt"`
}
type Job struct {
	ID         string    `json:"id"`
	SnapshotID string    `json:"snapshotId"`
	State      string    `json:"state"`
	Scale      int       `json:"scale"`
	Attempt    int       `json:"attempt"`
	Error      string    `json:"error,omitempty"`
	ArtifactID string    `json:"artifactId,omitempty"`
	BatchID    string    `json:"batchId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Payload    Snapshot  `json:"payload"`
}

// Batch is an atomically accepted set of same-snapshot render jobs. The whole
// set is committed in one transaction: either every member exists or none of
// them does. Its state is derived from the committed member states, so a
// restart never leaves a batch in an unexplainable state.
type Batch struct {
	ID         string    `json:"id"`
	SnapshotID string    `json:"snapshotId"`
	State      string    `json:"state"`
	JobIDs     []string  `json:"jobIds"`
	Scales     []int     `json:"scales"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
type Artifact struct {
	ID     string `json:"id"`
	JobID  string `json:"jobId"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	PNG    []byte `json:"png,omitempty"`
}
type Receipt struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	JobID       string `json:"jobId,omitempty"`
	BatchID     string `json:"batchId,omitempty"`
}
type Event struct {
	Sequence int       `json:"sequence"`
	Kind     string    `json:"kind"`
	Subject  string    `json:"subject"`
	At       time.Time `json:"at"`
}
type State struct {
	Version   int                 `json:"version"`
	Scenes    map[string]Scene    `json:"scenes"`
	Assets    map[string]Asset    `json:"assets"`
	Snapshots map[string]Snapshot `json:"snapshots"`
	Jobs      map[string]Job      `json:"jobs"`
	Artifacts map[string]Artifact `json:"artifacts"`
	Batches   map[string]Batch    `json:"batches"`
	Receipts  map[string]Receipt  `json:"receipts"`
	Events    []Event             `json:"events"`
	Sequence  int                 `json:"sequence"`
}

func NewState() State {
	return State{Version: FormatVersion, Scenes: map[string]Scene{}, Assets: map[string]Asset{}, Snapshots: map[string]Snapshot{}, Jobs: map[string]Job{}, Artifacts: map[string]Artifact{}, Batches: map[string]Batch{}, Receipts: map[string]Receipt{}}
}

// BatchStates are the lifecycle states of a batch.
const (
	BatchActive    = "active"    // at least one member is queued or running
	BatchSettled   = "settled"   // every member reached a terminal state
	BatchCompleted = "completed" // every member succeeded
)

// terminalJobState reports whether a job can no longer move on its own.
func terminalJobState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

// ReconcileBatches derives every batch state from its committed member jobs
// and emits audit events on transitions. It must be called inside the same
// transaction that changes member states, so a batch can never describe a set
// the jobs do not actually form.
func (s *State) ReconcileBatches() {
	for id, b := range s.Batches {
		next := BatchCompleted
		for _, jid := range b.JobIDs {
			j, ok := s.Jobs[jid]
			if !ok || !terminalJobState(j.State) {
				next = BatchActive
				break
			}
			if j.State != "succeeded" {
				next = BatchSettled
			}
		}
		if next == b.State {
			continue
		}
		b.State = next
		b.UpdatedAt = time.Now().UTC()
		s.Batches[id] = b
		if next == BatchActive {
			s.Emit("batch.reopened", id)
		} else {
			s.Emit("batch."+next, id)
		}
	}
}
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func CanonicalDigest(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return Digest(b)
}
func SnapshotDigest(s Snapshot) string {
	return CanonicalDigest(struct {
		Document Document
		Assets   map[string]Asset
	}{s.Document, s.Assets})
}
func Clone[T any](v T) T {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	var out T
	if e = json.Unmarshal(b, &out); e != nil {
		panic(e)
	}
	return out
}
func (s *State) Emit(kind, id string) {
	s.Sequence++
	s.Events = append(s.Events, Event{Sequence: s.Sequence, Kind: kind, Subject: id, At: time.Now().UTC()})
	if len(s.Events) > 500 {
		s.Events = s.Events[len(s.Events)-500:]
	}
}
func ID(prefix string, seq int) string { return fmt.Sprintf("%s-%08d", prefix, seq) }
