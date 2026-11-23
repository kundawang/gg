package text

import "errors"

// Sentinel errors for text package.
var (
	// ErrEmptyFontData is returned when font data is empty.
	ErrEmptyFontData = errors.New("text: empty font data")

	// ErrEmptyFaces is returned when no faces are provided to MultiFace.
	ErrEmptyFaces = errors.New("text: faces cannot be empty")

	// ErrFontSourceClosed is returned when an operation needs font data from a
	// FontSource that has already been closed. Faces created before Close
	// surface it through their error-returning operations instead of reading
	// released font bytes.
	ErrFontSourceClosed = errors.New("text: FontSource is closed")

	// ErrFontSourceNotParsed is returned when a FontSource has no parsed font
	// (for example a test double that intentionally fails parsing).
	ErrFontSourceNotParsed = errors.New("text: FontSource has no parsed font")
)

// DirectionMismatchError is returned when faces have different directions.
type DirectionMismatchError struct {
	Index    int
	Got      Direction
	Expected Direction
}

func (e *DirectionMismatchError) Error() string {
	return "text: face direction mismatch"
}
