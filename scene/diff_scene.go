package scene

import "fmt"

// DiffScenes compares two scenes' flattened encodings, supplying each scene's
// image registry size so image references are fully validated. It is the
// scene-level entry point for auditable frame-to-frame diffing; see
// DiffEncodings for the semantics of the returned SceneDiff.
//
// Neither scene is mutated and no shared cache is touched: the diff runs on
// cloned encoding snapshots rather than calling Scene.Encoding(), whose
// flattening step reorganizes a scene's internal layer state.
func DiffScenes(oldScene, newScene *Scene) (*SceneDiff, error) {
	if oldScene == nil || newScene == nil {
		which := "old"
		if newScene == nil {
			which = "new"
		}
		return nil, fmt.Errorf("%w: %s scene is nil", ErrNilEncoding, which)
	}
	oldEnc := snapshotEncoding(oldScene)
	newEnc := snapshotEncoding(newScene)
	return DiffEncodingsWithOptions(oldEnc, newEnc, DiffOptions{
		OldImageCount: len(oldScene.Images()),
		NewImageCount: len(newScene.Images()),
	})
}

// snapshotEncoding returns a cloned, flattened view of a scene's commands
// without modifying the scene. Already-flattened content lives in s.encoding;
// unflattened content lives in the layer stack. Both are copied into a fresh
// encoding in bottom-to-top playback order.
func snapshotEncoding(s *Scene) *Encoding {
	merged := s.encoding.Clone()
	for _, layer := range s.layerStack.All() {
		if layer.Encoding != nil && !layer.Encoding.IsEmpty() {
			merged.Append(layer.Encoding)
		}
	}
	return merged
}
