package model

// LayerUpsert creates a layer, or replaces an existing layer in place. When
// ParentID is set the layer is placed inside that group (an explicit empty
// string targets document roots); omitting it keeps an existing layer in its
// current parent and appends a new layer to the document roots.
type LayerUpsert struct {
	Layer    Layer   `json:"layer"`
	ParentID *string `json:"parentId,omitempty"`
}

// Parent returns the requested parent and whether the request names one.
func (u LayerUpsert) Parent() (string, bool) {
	if u.ParentID == nil {
		return "", false
	}
	return *u.ParentID, true
}

// LayerOrder replaces the child sequence of one parent. ParentID is empty for
// the root document layers.
type LayerOrder struct {
	ParentID string   `json:"parentId,omitempty"`
	LayerIDs []string `json:"layerIds"`
}

// SceneEdit is a layer-scoped scene revision. ExpectedRevision is the revision
// the client read before preparing the edit; the server merges the edit onto
// newer revisions instead of overwriting them.
type SceneEdit struct {
	ExpectedRevision int           `json:"expectedRevision"`
	Name             *string       `json:"name,omitempty"`
	Upserts          []LayerUpsert `json:"layers,omitempty"`
	Deletes          []string      `json:"deleteLayers,omitempty"`
	Orders           []LayerOrder  `json:"order,omitempty"`
}

func (e SceneEdit) Empty() bool {
	return e.Name == nil && len(e.Upserts) == 0 && len(e.Deletes) == 0 && len(e.Orders) == 0
}
