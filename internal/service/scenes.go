package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/scene"
	"strings"
	"time"
)

type Service struct{ Store *persist.Store }

// EditResult reports the outcome of a layer-scoped revision.
type EditResult struct {
	Scene        model.Scene `json:"scene"`
	BaseRevision int         `json:"baseRevision"`
	Merged       bool        `json:"merged"`
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 160
}

// recordRevision keeps the documents needed to merge stale edits. Every
// revision is retained up to MaxSceneHistory; edits older than that must be
// rebased by the client because their base document is gone.
func recordRevision(st *model.State, v model.Scene) {
	if st.Revisions == nil {
		st.Revisions = map[string][]model.SceneRevision{}
	}
	history := st.Revisions[v.ID]
	if n := len(history); n > 0 && history[n-1].Revision == v.Revision && model.CanonicalDigest(history[n-1].Document) == model.CanonicalDigest(v.Document) && history[n-1].Name == v.Name {
		history[n-1] = model.SceneRevision{Revision: v.Revision, Name: v.Name, Document: v.Document}
		st.Revisions[v.ID] = history
		return
	}
	history = append(history, model.SceneRevision{Revision: v.Revision, Name: v.Name, Document: v.Document})
	if len(history) > model.MaxSceneHistory {
		history = history[len(history)-model.MaxSceneHistory:]
	}
	st.Revisions[v.ID] = history
}

// revisionDocument returns the document a client saw at revision. Revisions
// whose document is unchanged by state transitions are recorded, so the exact
// base can always be recovered while it remains inside the retained history.
func revisionDocument(st *model.State, id string, revision int) (model.Document, bool) {
	history := st.Revisions[id]
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Revision == revision {
			return history[i].Document, true
		}
	}
	return model.Document{}, false
}

func checkAssetClosure(st *model.State, d model.Document) error {
	for _, ref := range scene.References(d) {
		a, ok := st.Assets[ref]
		if !ok || a.State != "available" {
			return model.Conflict("scene references unavailable asset")
		}
	}
	return nil
}

// PutScene replaces the whole document. It stays the unconditional write path
// for integrations that manage the complete document themselves; stale
// revisions are rejected rather than merged.
func (s *Service) PutScene(id, name string, expected int, d model.Document) (model.Scene, error) {
	var out model.Scene
	if !scene.ValidID(id) || !validName(name) {
		return out, model.Invalid("invalid scene id or name")
	}
	if e := scene.Validate(d); e != nil {
		return out, e
	}
	e := s.Store.Update(func(st *model.State) error {
		old, ok := st.Scenes[id]
		if old.Revision != expected {
			return model.Conflict("scene revision changed")
		}
		if ok && old.State != "active" {
			return model.Conflict("scene is archived")
		}
		if e := checkAssetClosure(st, d); e != nil {
			return e
		}
		out = model.Scene{ID: id, Name: name, Revision: old.Revision + 1, State: "active", Document: d, UpdatedAt: time.Now().UTC()}
		st.Scenes[id] = out
		recordRevision(st, out)
		st.Emit("scene.saved", id)
		return nil
	})
	return out, e
}

// EditScene applies a layer-scoped edit. Edits against a stale base are merged
// with the layers other editors committed in the meantime. Conflicting layer
// modifications, deletions, moves and orderings are reported instead of
// overwriting the committed document.
func (s *Service) EditScene(id string, edit model.SceneEdit) (EditResult, error) {
	var out EditResult
	if !scene.ValidID(id) {
		return out, model.Invalid("invalid scene id")
	}
	if edit.Name != nil && !validName(*edit.Name) {
		return out, model.Invalid("invalid scene name")
	}
	e := s.Store.Update(func(st *model.State) error {
		return applyEdit(st, id, edit, &out)
	})
	if e != nil {
		return EditResult{}, e
	}
	return out, nil
}

func applyEdit(st *model.State, id string, edit model.SceneEdit, out *EditResult) error {
	current, ok := st.Scenes[id]
	if !ok {
		return model.Missing("scene not found")
	}
	if current.State != "active" {
		return model.Conflict("scene is archived")
	}
	if edit.ExpectedRevision > current.Revision {
		return model.Conflict("expectedRevision is ahead of the committed scene")
	}
	base, ok := revisionDocument(st, id, edit.ExpectedRevision)
	if !ok {
		if edit.ExpectedRevision != current.Revision {
			return model.RevisionConflict(edit.ExpectedRevision, current.Revision, []model.ConflictDetail{{
				Reason:  "base-unavailable",
				Message: "edit base revision is older than retained scene history; refresh the scene and resubmit",
			}})
		}
		// The caller is up to date even though its revision document was not
		// retained; the current document is that base by definition.
		base = current.Document
	}
	merged, conflicts, e := scene.Merge(base, current.Document, edit)
	if e != nil {
		return e
	}
	if len(conflicts) > 0 {
		return model.RevisionConflict(edit.ExpectedRevision, current.Revision, conflicts)
	}
	if e := checkAssetClosure(st, merged); e != nil {
		return e
	}
	name := current.Name
	if edit.Name != nil {
		name = *edit.Name
	}
	if name == current.Name && model.CanonicalDigest(merged) == model.CanonicalDigest(current.Document) {
		// Replaying an already committed edit produces no revision; freezing
		// afterwards still captures exactly the document the caller meant.
		out.Scene = current
		out.BaseRevision = edit.ExpectedRevision
		out.Merged = current.Revision != edit.ExpectedRevision
		return nil
	}
	next := model.Scene{ID: id, Name: name, Revision: current.Revision + 1, State: "active", Document: merged, UpdatedAt: time.Now().UTC()}
	st.Scenes[id] = next
	recordRevision(st, next)
	st.Emit("scene.edited", id)
	out.Scene = next
	out.BaseRevision = edit.ExpectedRevision
	out.Merged = current.Revision != edit.ExpectedRevision
	return nil
}

func (s *Service) SceneState(id string, expected int, target string) (model.Scene, error) {
	var out model.Scene
	if target != "active" && target != "archived" {
		return out, model.Invalid("unknown scene state")
	}
	e := s.Store.Update(func(st *model.State) error {
		v, ok := st.Scenes[id]
		if !ok {
			return model.Missing("scene not found")
		}
		if v.Revision != expected {
			return model.Conflict("scene revision changed")
		}
		if v.State == target {
			out = v
			return nil
		}
		v.State = target
		v.Revision++
		v.UpdatedAt = time.Now().UTC()
		st.Scenes[id] = v
		recordRevision(st, v)
		st.Emit("scene."+target, id)
		out = v
		return nil
	})
	return out, e
}

// Freeze captures one committed scene revision. Callers pass the revision they
// observed; a concurrent revision makes the freeze fail instead of capturing a
// document that mixes edits the caller never saw.
func (s *Service) Freeze(id string, expected int) (model.Snapshot, error) {
	var out model.Snapshot
	e := s.Store.Update(func(st *model.State) error {
		var e error
		out, e = freezeScene(st, id, expected)
		return e
	})
	return out, e
}

// FreezeEdit merges a layer-scoped edit and freezes the result in a single
// state transaction, so the snapshot can never combine the pre-merge document
// with a later revision. The caller must reference the revision it actually
// observed; the edit itself may be based on an older revision and is merged
// onto that observed revision. A conflicting edit freezes nothing.
func (s *Service) FreezeEdit(id string, expected int, edit model.SceneEdit) (model.Snapshot, error) {
	var out model.Snapshot
	if edit.Name != nil && !validName(*edit.Name) {
		return out, model.Invalid("invalid scene name")
	}
	e := s.Store.Update(func(st *model.State) error {
		v, ok := st.Scenes[id]
		if !ok {
			return model.Missing("scene not found")
		}
		if v.Revision != expected {
			return model.Conflict("scene revision changed")
		}
		if v.State != "active" {
			return model.Conflict("scene is archived")
		}
		var result EditResult
		if e := applyEdit(st, id, edit, &result); e != nil {
			return e
		}
		var e error
		out, e = freezeScene(st, id, st.Scenes[id].Revision)
		return e
	})
	return out, e
}

func freezeScene(st *model.State, id string, expected int) (model.Snapshot, error) {
	var out model.Snapshot
	v, ok := st.Scenes[id]
	if !ok {
		return out, model.Missing("scene not found")
	}
	if v.Revision != expected {
		return out, model.Conflict("scene revision changed")
	}
	if v.State != "active" {
		return out, model.Conflict("scene revision or state changed")
	}
	assets := map[string]model.Asset{}
	for _, ref := range scene.References(v.Document) {
		a, ok := st.Assets[ref]
		if !ok || a.State != "available" {
			return out, model.Conflict("snapshot references unavailable asset")
		}
		assets[ref] = a
	}
	out = model.Snapshot{ID: model.ID("snap", st.Sequence+1), SceneID: id, Revision: v.Revision, State: "active", Document: v.Document, Assets: assets, CreatedAt: time.Now().UTC()}
	out.Digest = model.SnapshotDigest(out)
	st.Snapshots[out.ID] = out
	st.Emit("snapshot.frozen", out.ID)
	return out, nil
}

func (s *Service) Revoke(id string) error {
	return s.Store.Update(func(st *model.State) error {
		v, ok := st.Snapshots[id]
		if !ok {
			return model.Missing("snapshot not found")
		}
		if v.State == "revoked" {
			return nil
		}
		v.State = "revoked"
		st.Snapshots[id] = v
		st.Emit("snapshot.revoked", id)
		return nil
	})
}
