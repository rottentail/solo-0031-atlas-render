package persist

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"atlas-render/internal/scene"
	"fmt"
)

func Validate(s model.State) error {
	if s.Version != model.FormatVersion {
		return fmt.Errorf("unsupported state version %d", s.Version)
	}
	if s.Scenes == nil || s.Assets == nil || s.Snapshots == nil || s.Jobs == nil || s.Artifacts == nil || s.Batches == nil || s.Receipts == nil {
		return fmt.Errorf("state maps must be present")
	}
	for id, v := range s.Scenes {
		if id != v.ID || !scene.ValidID(id) || v.Revision < 1 || (v.State != "active" && v.State != "archived") {
			return fmt.Errorf("invalid scene record %s", id)
		}
		if e := scene.Validate(v.Document); e != nil {
			return e
		}
	}
	for id, v := range s.Assets {
		if id != v.ID || !scene.ValidID(id) || (v.State != "available" && v.State != "retired") {
			return fmt.Errorf("invalid asset record %s", id)
		}
		if e := ValidateAsset(v); e != nil {
			return e
		}
	}
	for id, v := range s.Snapshots {
		if id != v.ID || (v.State != "active" && v.State != "revoked") {
			return fmt.Errorf("invalid snapshot record %s", id)
		}
		if _, ok := s.Scenes[v.SceneID]; !ok {
			return fmt.Errorf("orphan snapshot %s", id)
		}
		if e := ValidateSnapshot(v); e != nil {
			return e
		}
	}
	for id, j := range s.Jobs {
		if id != j.ID || j.SnapshotID != j.Payload.ID || j.Scale < 1 || j.Scale > 2 || j.Attempt < 1 {
			return fmt.Errorf("invalid job %s", id)
		}
		if _, ok := s.Snapshots[j.SnapshotID]; !ok {
			return fmt.Errorf("orphan job %s", id)
		}
		if j.BatchID != "" {
			b, ok := s.Batches[j.BatchID]
			if !ok {
				return fmt.Errorf("job %s references missing batch", id)
			}
			if !contains(b.JobIDs, id) || b.SnapshotID != j.SnapshotID {
				return fmt.Errorf("job %s is not a member of its batch", id)
			}
		}
		switch j.State {
		case "queued", "running", "failed", "cancelled":
			if j.ArtifactID != "" {
				return fmt.Errorf("unfinished job references artifact")
			}
		case "succeeded":
			a, ok := s.Artifacts[j.ArtifactID]
			if !ok || a.JobID != id {
				return fmt.Errorf("job result missing")
			}
		default:
			return fmt.Errorf("invalid job state")
		}
		if e := ValidateSnapshot(j.Payload); e != nil {
			return e
		}
	}
	for id, b := range s.Batches {
		if id != b.ID || !scene.ValidID(id) || len(b.JobIDs) < 1 || len(b.JobIDs) != len(b.Scales) {
			return fmt.Errorf("invalid batch %s", id)
		}
		if b.State != model.BatchActive && b.State != model.BatchSettled && b.State != model.BatchCompleted {
			return fmt.Errorf("invalid batch state %s", id)
		}
		if _, ok := s.Snapshots[b.SnapshotID]; !ok {
			return fmt.Errorf("orphan batch %s", id)
		}
		seenScales := map[int]bool{}
		active := false
		for i, jid := range b.JobIDs {
			j, ok := s.Jobs[jid]
			if !ok || j.BatchID != id || j.SnapshotID != b.SnapshotID || j.Scale != b.Scales[i] {
				return fmt.Errorf("batch %s member mismatch", id)
			}
			if seenScales[j.Scale] {
				return fmt.Errorf("batch %s repeats a scale", id)
			}
			seenScales[j.Scale] = true
			if j.State == "queued" || j.State == "running" {
				active = true
			}
		}
		if active != (b.State == model.BatchActive) {
			return fmt.Errorf("batch %s state disagrees with its members", id)
		}
		if b.State == model.BatchCompleted {
			for _, jid := range b.JobIDs {
				if s.Jobs[jid].State != "succeeded" {
					return fmt.Errorf("completed batch %s has an unsuccessful member", id)
				}
			}
		}
	}
	for id, a := range s.Artifacts {
		j, ok := s.Jobs[a.JobID]
		if id != a.ID || !ok || j.ArtifactID != id || j.State != "succeeded" {
			return fmt.Errorf("orphan artifact %s", id)
		}
		switch a.State {
		case "live":
			if model.Digest(a.PNG) != a.Digest {
				return fmt.Errorf("artifact digest mismatch")
			}
		case "purged":
			if len(a.PNG) != 0 {
				return fmt.Errorf("purged artifact retains bytes")
			}
		default:
			return fmt.Errorf("invalid artifact state")
		}
	}
	for key, r := range s.Receipts {
		if key != r.Key || r.Fingerprint == "" {
			return fmt.Errorf("invalid receipt")
		}
		targets := 0
		if r.JobID != "" {
			if _, ok := s.Jobs[r.JobID]; !ok {
				return fmt.Errorf("receipt job missing")
			}
			targets++
		}
		if r.BatchID != "" {
			if _, ok := s.Batches[r.BatchID]; !ok {
				return fmt.Errorf("receipt batch missing")
			}
			targets++
		}
		if targets != 1 {
			return fmt.Errorf("receipt must target exactly one job or batch")
		}
	}
	return nil
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
func ValidateAsset(a model.Asset) error {
	if model.Digest(a.PNG) != a.Digest {
		return model.Invalid("asset digest mismatch")
	}
	img, e := codec.Decode(a.PNG)
	if e != nil {
		return e
	}
	if img.Bounds().Dx() != a.Width || img.Bounds().Dy() != a.Height {
		return model.Invalid("asset dimensions mismatch")
	}
	return nil
}
func ValidateSnapshot(v model.Snapshot) error {
	if e := scene.Validate(v.Document); e != nil {
		return e
	}
	if model.SnapshotDigest(v) != v.Digest {
		return model.Invalid("snapshot digest mismatch")
	}
	refs := scene.References(v.Document)
	if len(refs) != len(v.Assets) {
		return model.Invalid("snapshot asset closure mismatch")
	}
	for _, id := range refs {
		a, ok := v.Assets[id]
		if !ok || id != a.ID {
			return model.Invalid("snapshot asset missing")
		}
		if e := ValidateAsset(a); e != nil {
			return e
		}
	}
	return nil
}
