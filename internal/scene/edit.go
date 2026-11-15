package scene

import (
	"atlas-render/internal/model"
	"sort"
)

// documentIndex maps every layer in a document to its content, its parent id
// ("" for document roots) and the ordered child ids of every parent.
type documentIndex struct {
	layers map[string]model.Layer
	parent map[string]string
	order  map[string][]string
}

func buildIndex(d model.Document) documentIndex {
	idx := documentIndex{layers: map[string]model.Layer{}, parent: map[string]string{}, order: map[string][]string{}}
	var walk func([]model.Layer, string)
	walk = func(layers []model.Layer, parent string) {
		ids := make([]string, 0, len(layers))
		for _, l := range layers {
			idx.layers[l.ID] = l
			idx.parent[l.ID] = parent
			ids = append(ids, l.ID)
			walk(l.Children, l.ID)
		}
		idx.order[parent] = ids
	}
	walk(d.Layers, "")
	return idx
}

func sameLayer(a, b model.Layer) bool {
	return model.CanonicalDigest(a) == model.CanonicalDigest(b)
}

// ownFields returns a copy of the layer without its children. Layer-local
// properties and the group's child structure are merged independently: a
// transform edit on a group must not collide with a concurrent edit below it.
func ownFields(l model.Layer) model.Layer {
	l.Children = nil
	return l
}

func sameOwnFields(a, b model.Layer) bool {
	return model.CanonicalDigest(ownFields(a)) == model.CanonicalDigest(ownFields(b))
}

func sameChildren(a, b model.Layer) bool {
	return model.CanonicalDigest(a.Children) == model.CanonicalDigest(b.Children)
}

// modifiedField names what a concurrent editor changed on one layer.
func modifiedField(a, b model.Layer) string {
	a, b = ownFields(a), ownFields(b)
	transform := model.CanonicalDigest(a.Transform) != model.CanonicalDigest(b.Transform)
	a.Transform, b.Transform = model.Transform{}, model.Transform{}
	content := model.CanonicalDigest(a) != model.CanonicalDigest(b)
	switch {
	case transform && content:
		return "transform+content"
	case transform:
		return "transform"
	default:
		return "content"
	}
}

func validateEdit(e model.SceneEdit) error {
	if e.ExpectedRevision < 0 {
		return model.Invalid("expectedRevision must be zero or positive")
	}
	if e.Empty() {
		return model.Invalid("edit does not contain a name, layer, deletion or order change")
	}
	upserts := map[string]bool{}
	for _, u := range e.Upserts {
		id := u.Layer.ID
		if !ValidID(id) {
			return model.Invalid("invalid layer id in edit")
		}
		if upserts[id] {
			return model.Invalid("layer %s is upserted more than once", id)
		}
		upserts[id] = true
		if parent, ok := u.Parent(); ok {
			if parent != "" && !ValidID(parent) {
				return model.Invalid("invalid parent id for layer %s", id)
			}
			if parent == id {
				return model.Invalid("layer %s cannot be its own parent", id)
			}
		}
	}
	deletes := map[string]bool{}
	for _, id := range e.Deletes {
		if !ValidID(id) {
			return model.Invalid("invalid layer id in deleteLayers")
		}
		if deletes[id] {
			return model.Invalid("layer %s is deleted more than once", id)
		}
		if upserts[id] {
			return model.Invalid("layer %s cannot be modified and deleted in the same edit", id)
		}
		deletes[id] = true
	}
	parents := map[string]bool{}
	for _, o := range e.Orders {
		if o.ParentID != "" && !ValidID(o.ParentID) {
			return model.Invalid("invalid parent id in order")
		}
		if parents[o.ParentID] {
			return model.Invalid("order for parent %q is specified more than once", o.ParentID)
		}
		parents[o.ParentID] = true
		seen := map[string]bool{}
		for _, id := range o.LayerIDs {
			if !ValidID(id) {
				return model.Invalid("invalid layer id in order")
			}
			if seen[id] {
				return model.Invalid("order lists layer %s more than once", id)
			}
			seen[id] = true
		}
	}
	return nil
}

func subtreeIDs(idx documentIndex, id string) []string {
	out := []string{id}
	for _, child := range idx.order[id] {
		out = append(out, subtreeIDs(idx, child)...)
	}
	return out
}

func layerIDsIn(l model.Layer) []string {
	out := []string{l.ID}
	for _, child := range l.Children {
		out = append(out, layerIDsIn(child)...)
	}
	return out
}

// isDescendant reports whether node lives inside the subtree rooted at
// ancestor. A move that would place a group below itself is rejected.
func isDescendant(idx documentIndex, node, ancestor string) bool {
	for current := idx.parent[node]; current != ""; current = idx.parent[current] {
		if current == ancestor {
			return true
		}
	}
	return false
}

func introducedCollision(idx documentIndex, l model.Layer) string {
	if _, ok := idx.layers[l.ID]; ok {
		return l.ID
	}
	for _, child := range l.Children {
		if id := introducedCollision(idx, child); id != "" {
			return id
		}
	}
	return ""
}

func filterIDs(ids []string, keep map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if keep[id] {
			out = append(out, id)
		}
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Merge applies a layer-scoped edit on top of the current document. Base is
// the revision the editor read; current is the latest committed document.
// Concurrent changes to unrelated layers are preserved. Changes that cannot be
// combined are reported as explicit conflicts; the returned document is then
// not committed by the caller.
func Merge(base, current model.Document, e model.SceneEdit) (model.Document, []model.ConflictDetail, error) {
	if err := validateEdit(e); err != nil {
		return model.Document{}, nil, err
	}
	b, c := buildIndex(base), buildIndex(current)
	conflicts := []model.ConflictDetail{}
	add := func(layer, parent, reason, field, message string) {
		conflicts = append(conflicts, model.ConflictDetail{Layer: layer, Parent: parent, Reason: reason, Field: field, Message: message})
	}

	// Expand deletions up front so that modifications below a simultaneously
	// deleted group are rejected instead of resurrecting part of the subtree.
	deleted := map[string]bool{}
	for _, id := range e.Deletes {
		if _, inBase := b.layers[id]; !inBase {
			return model.Document{}, nil, model.Invalid("delete references unknown layer %s", id)
		}
		for _, part := range subtreeIDs(b, id) {
			deleted[part] = true
		}
	}
	for _, u := range e.Upserts {
		for _, id := range layerIDsIn(u.Layer) {
			if deleted[id] {
				return model.Document{}, nil, model.Invalid("layer %s is inside a group deleted by the same edit", id)
			}
		}
		if parent, ok := u.Parent(); ok && deleted[parent] {
			return model.Document{}, nil, model.Invalid("parent %s is deleted by the same edit", parent)
		}
	}
	for _, o := range e.Orders {
		if deleted[o.ParentID] {
			return model.Document{}, nil, model.Invalid("order references parent %s deleted by the same edit", o.ParentID)
		}
		for _, id := range o.LayerIDs {
			if deleted[id] {
				return model.Document{}, nil, model.Invalid("order references layer %s deleted by the same edit", id)
			}
		}
	}

	working := model.Clone(current)
	w := buildIndex(working)

	// Deletions.
	for _, id := range e.Deletes {
		baseLayer := b.layers[id]
		currentLayer, inCurrent := c.layers[id]
		if !inCurrent {
			continue // already removed; the intent agrees
		}
		if !sameOwnFields(baseLayer, currentLayer) {
			add(id, c.parent[id], "layer-modified", modifiedField(baseLayer, currentLayer), "layer changed after the edit base revision")
			continue
		}
		if !sameChildren(baseLayer, currentLayer) {
			add(id, c.parent[id], "layer-children-modified", "children", "nested layers changed after the edit base revision")
			continue
		}
		if c.parent[id] != b.parent[id] {
			add(id, c.parent[id], "layer-moved", "", "layer was moved after the edit base revision")
			continue
		}
		working.Layers, _ = removeLayer(working.Layers, id)
		w = buildIndex(working)
	}

	// Updates of layers that existed at the base revision, and replays of
	// creations committed by an earlier attempt of the same edit.
	pending := []model.LayerUpsert{}
	for _, u := range e.Upserts {
		id := u.Layer.ID
		baseLayer, inBase := b.layers[id]
		currentLayer, inCurrent := c.layers[id]
		switch {
		case inBase && inCurrent:
			target, moved := u.Parent()
			// An entry that repeats the base state carries no intent of its
			// own; keep whatever the other editor committed.
			if !sameOwnFields(baseLayer, u.Layer) || (moved && target != b.parent[id]) {
				// A concurrent editor touched this layer. If the committed
				// layer already carries exactly the submitted state, this is a
				// replay of an earlier attempt and not a conflict.
				replay := sameOwnFields(currentLayer, u.Layer)
				if !replay && !sameOwnFields(baseLayer, currentLayer) {
					add(id, c.parent[id], "layer-modified", modifiedField(baseLayer, currentLayer), "layer changed after the edit base revision")
					continue
				}
				if moved && c.parent[id] != b.parent[id] && target != c.parent[id] {
					add(id, c.parent[id], "layer-moved", "", "layer was moved after the edit base revision")
					continue
				}
				// Child lists are merged through their own upserts, deletions and
				// order entries. A stale copy of a group must not overwrite the
				// children other editors committed: an inline child list is only
				// accepted when it matches what the base or current revision
				// already contains.
				if len(u.Layer.Children) > 0 && !sameChildren(baseLayer, u.Layer) && !sameChildren(currentLayer, u.Layer) {
					return model.Document{}, nil, model.Invalid("layer %s: changing an existing layer must not rewrite its child list; edit, delete or reorder the nested layers directly", id)
				}
				replacement := model.Clone(u.Layer)
				if replay {
					replacement = model.Clone(currentLayer)
				}
				// A group node keeps the committed child order; children are only
				// changed through their own entries.
				replacement.Children = currentLayer.Children
				if replacement.Kind != "group" {
					replacement.Children = nil
				}
				working.Layers, _ = replaceLayer(working.Layers, replacement)
				w = buildIndex(working)
			} else {
				target, moved = "", false
			}
			if moved && target != w.parent[id] {
				if target == "" {
					layers, ok := moveLayer(working.Layers, id, "")
					if !ok {
						return model.Document{}, nil, model.Invalid("layer %s cannot be moved", id)
					}
					working.Layers = layers
					w = buildIndex(working)
					continue
				}
				parent, ok := w.layers[target]
				if !ok || parent.Kind != "group" {
					return model.Document{}, nil, model.Invalid("parent %s is not a group reachable for layer %s", target, id)
				}
				if isDescendant(w, target, id) {
					return model.Document{}, nil, model.Invalid("layer %s cannot be moved into its own subtree", id)
				}
				layers, ok := moveLayer(working.Layers, id, target)
				if !ok {
					return model.Document{}, nil, model.Invalid("parent %s is not a group reachable for layer %s", target, id)
				}
				working.Layers = layers
				w = buildIndex(working)
			}
		case inBase && !inCurrent:
			add(id, c.parent[id], "layer-deleted", "", "layer was deleted after the edit base revision")
		case !inBase && inCurrent:
			target, requested := u.Parent()
			if !requested {
				target = ""
			}
			if sameLayer(currentLayer, u.Layer) && target == c.parent[id] {
				continue // replay of an already committed creation
			}
			add(id, c.parent[id], "layer-created", "", "layer id was created concurrently with different content")
		default:
			pending = append(pending, u)
		}
	}

	// Insertions of new layers, allowing parents that are themselves part of
	// the same edit.
	for len(pending) > 0 {
		progressed := false
		next := []model.LayerUpsert{}
		for _, u := range pending {
			id := u.Layer.ID
			target, _ := u.Parent()
			if existing, ok := w.layers[id]; ok {
				if sameLayer(existing, u.Layer) && w.parent[id] == target {
					progressed = true
					continue
				}
				return model.Document{}, nil, model.Invalid("layer %s is introduced more than once", id)
			}
			if collision := introducedCollision(w, u.Layer); collision != "" {
				add(collision, c.parent[collision], "layer-created", "", "layer id in the new subtree was created concurrently")
				progressed = true
				continue
			}
			if target == "" {
				working.Layers = append(working.Layers, u.Layer)
				w = buildIndex(working)
				progressed = true
				continue
			}
			parent, ok := w.layers[target]
			if ok {
				if parent.Kind != "group" {
					return model.Document{}, nil, model.Invalid("parent %s is not a group", target)
				}
				layers, found := appendLayer(working.Layers, target, u.Layer)
				if !found {
					return model.Document{}, nil, model.Invalid("parent %s is not reachable", target)
				}
				working.Layers = layers
				w = buildIndex(working)
				progressed = true
				continue
			}
			if _, wasInBase := b.layers[target]; wasInBase {
				if _, stillInCurrent := c.layers[target]; !stillInCurrent {
					add(id, target, "parent-deleted", "", "parent group was deleted after the edit base revision")
					progressed = true
					continue
				}
			}
			next = append(next, u)
		}
		if !progressed {
			return model.Document{}, nil, model.Invalid("new layer parent is unknown or forms a cycle")
		}
		pending = next
	}

	// Reordering.
	for _, o := range e.Orders {
		if o.ParentID != "" {
			parent, ok := w.layers[o.ParentID]
			if !ok {
				if _, wasInBase := b.layers[o.ParentID]; wasInBase {
					if _, stillInCurrent := c.layers[o.ParentID]; !stillInCurrent {
						add("", o.ParentID, "parent-deleted", "", "parent group was deleted after the edit base revision")
						continue
					}
				}
				return model.Document{}, nil, model.Invalid("order references unknown parent %s", o.ParentID)
			}
			if parent.Kind != "group" {
				return model.Document{}, nil, model.Invalid("order parent %s is not a group", o.ParentID)
			}
		}
		common := map[string]bool{}
		for _, id := range b.order[o.ParentID] {
			if deleted[id] {
				continue
			}
			if _, inCurrent := c.layers[id]; inCurrent {
				common[id] = true
			}
		}
		committed := filterIDs(c.order[o.ParentID], common)
		if !sameIDs(filterIDs(b.order[o.ParentID], common), committed) {
			// The same sibling sequence may already be committed by an
			// earlier attempt of this edit; that is not a conflict.
			replay := sameIDs(o.LayerIDs, committed)
			if !replay {
				add("", o.ParentID, "order-modified", "", "children were reordered after the edit base revision")
				continue
			}
		}
		bad := false
		seen := map[string]bool{}
		for _, id := range o.LayerIDs {
			seen[id] = true
			if _, ok := w.layers[id]; !ok {
				if _, wasInBase := b.layers[id]; wasInBase {
					add(id, o.ParentID, "layer-deleted", "", "ordered layer was deleted after the edit base revision")
				} else {
					return model.Document{}, nil, model.Invalid("order references unknown layer %s", id)
				}
				bad = true
				continue
			}
			if w.parent[id] != o.ParentID {
				return model.Document{}, nil, model.Invalid("layer %s is not a child of %q", id, o.ParentID)
			}
		}
		if bad {
			continue
		}
		for _, id := range b.order[o.ParentID] {
			if deleted[id] || seen[id] {
				continue
			}
			if _, stillInWorking := w.layers[id]; !stillInWorking {
				continue
			}
			return model.Document{}, nil, model.Invalid("order must list existing child %s of %q", id, o.ParentID)
		}
		// Children that appeared after the base revision stay visible at the
		// end of the parent instead of being silently dropped.
		sequence := append([]string{}, o.LayerIDs...)
		for _, id := range w.order[o.ParentID] {
			if !seen[id] {
				sequence = append(sequence, id)
			}
		}
		if sameIDs(w.order[o.ParentID], sequence) {
			continue // already applied
		}
		layers, ok := reorderLayers(working.Layers, o.ParentID, sequence)
		if !ok {
			return model.Document{}, nil, model.Invalid("order cannot be applied to parent %q", o.ParentID)
		}
		working.Layers = layers
		w = buildIndex(working)
	}

	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].Layer != conflicts[j].Layer {
				return conflicts[i].Layer < conflicts[j].Layer
			}
			if conflicts[i].Parent != conflicts[j].Parent {
				return conflicts[i].Parent < conflicts[j].Parent
			}
			if conflicts[i].Reason != conflicts[j].Reason {
				return conflicts[i].Reason < conflicts[j].Reason
			}
			return conflicts[i].Field < conflicts[j].Field
		})
		return working, conflicts, nil
	}
	if err := Validate(working); err != nil {
		return model.Document{}, nil, err
	}
	return working, nil, nil
}

func removeLayer(layers []model.Layer, id string) ([]model.Layer, bool) {
	for i := range layers {
		if layers[i].ID == id {
			return append(layers[:i:i], layers[i+1:]...), true
		}
		if len(layers[i].Children) > 0 {
			if children, ok := removeLayer(layers[i].Children, id); ok {
				layers[i].Children = children
				return layers, true
			}
		}
	}
	return layers, false
}

func replaceLayer(layers []model.Layer, layer model.Layer) ([]model.Layer, bool) {
	for i := range layers {
		if layers[i].ID == layer.ID {
			layers[i] = layer
			return layers, true
		}
		if len(layers[i].Children) > 0 {
			if children, ok := replaceLayer(layers[i].Children, layer); ok {
				layers[i].Children = children
				return layers, true
			}
		}
	}
	return layers, false
}

// moveLayer detaches a layer from its current parent and appends it to the
// named group. The caller has already validated that the destination is not
// inside the moved subtree.
func moveLayer(layers []model.Layer, id, parentID string) ([]model.Layer, bool) {
	var moved model.Layer
	found := false
	var detach func([]model.Layer) []model.Layer
	detach = func(ls []model.Layer) []model.Layer {
		for i := range ls {
			if ls[i].ID == id {
				moved, found = ls[i], true
				return append(ls[:i:i], ls[i+1:]...)
			}
			if len(ls[i].Children) > 0 {
				ls[i].Children = detach(ls[i].Children)
			}
		}
		return ls
	}
	layers = detach(layers)
	if !found {
		return layers, false
	}
	if parentID == "" {
		return append(layers, moved), true
	}
	return appendLayer(layers, parentID, moved)
}

func appendLayer(layers []model.Layer, parentID string, layer model.Layer) ([]model.Layer, bool) {
	for i := range layers {
		if layers[i].ID == parentID {
			layers[i].Children = append(layers[i].Children, layer)
			return layers, true
		}
		if len(layers[i].Children) > 0 {
			if children, ok := appendLayer(layers[i].Children, parentID, layer); ok {
				layers[i].Children = children
				return layers, true
			}
		}
	}
	return layers, false
}

func reorderLayers(layers []model.Layer, parentID string, ids []string) ([]model.Layer, bool) {
	if parentID == "" {
		byID := map[string]model.Layer{}
		for _, l := range layers {
			byID[l.ID] = l
		}
		next := make([]model.Layer, 0, len(ids))
		for _, id := range ids {
			l, ok := byID[id]
			if !ok {
				return layers, false
			}
			next = append(next, l)
		}
		return next, true
	}
	for i := range layers {
		if layers[i].ID == parentID {
			byID := map[string]model.Layer{}
			for _, l := range layers[i].Children {
				byID[l.ID] = l
			}
			next := make([]model.Layer, 0, len(ids))
			for _, id := range ids {
				l, ok := byID[id]
				if !ok {
					return layers, false
				}
				next = append(next, l)
			}
			layers[i].Children = next
			return layers, true
		}
		if len(layers[i].Children) > 0 {
			if children, ok := reorderLayers(layers[i].Children, parentID, ids); ok {
				layers[i].Children = children
				return layers, true
			}
		}
	}
	return layers, false
}
