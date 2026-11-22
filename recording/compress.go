package recording

import (
	"math"
	"reflect"
)

// CompressOptions controls command compression for a [Recording].
type CompressOptions struct {
	// Enabled gates the compression pass. When false, [Compress] returns the
	// input recording unchanged together with a zero [CompressionStats],
	// providing a stable compatibility escape hatch for callers that need
	// the exact original command stream.
	Enabled bool
}

// CompressionStats reports what a compression pass removed and why each
// retained command survived. The counters always satisfy:
//
//	OriginalCommands   == CompressedCommands + RemovedCommands
//	RemovedCommands    == RedundantCommands + SupersededCommands
//	CompressedCommands == KeptBarrier + KeptLastWrite + KeptUnknown
type CompressionStats struct {
	// OriginalCommands is the number of commands in the input recording.
	OriginalCommands int
	// CompressedCommands is the number of commands in the output recording.
	CompressedCommands int

	// RemovedCommands equals OriginalCommands minus CompressedCommands.
	RemovedCommands int
	// RedundantCommands counts removed commands whose payload exactly
	// repeated the previous write to the same state slot.
	RedundantCommands int
	// SupersededCommands counts removed commands overwritten by a later,
	// different write to the same state slot within the same run.
	SupersededCommands int

	// KeptBarrier counts retained structural commands: Save/Restore,
	// clip-set commands and every drawing command. They delimit runs and
	// can never be removed or reordered across.
	KeptBarrier int
	// KeptLastWrite counts retained state commands that are the final
	// write of their state slot within a run.
	KeptLastWrite int
	// KeptUnknown counts retained nil or unknown-typed commands. The
	// compressor never removes a command it does not understand.
	KeptUnknown int

	// NonFiniteMatrices counts SetTransform commands carrying NaN or
	// infinite components. Such commands still compress under exact
	// last-write semantics; the counter only makes them observable.
	NonFiniteMatrices int
	// UnbalancedRestore counts Restore commands encountered while the
	// tracked Save/Restore stack was already empty.
	UnbalancedRestore int
	// MaxStackDepth is the deepest Save/Restore nesting observed.
	MaxStackDepth int
}

// stateSlot identifies an independently overwritable graphics-state slot.
// Every compressible state command is an absolute "set" operation, so within
// a run of consecutive state commands only the final write to each slot is
// observable.
type stateSlot uint8

const (
	slotNone stateSlot = iota
	slotTransform
	slotFillStyle
	slotStrokeStyle
	slotLineWidth
	slotLineCap
	slotLineJoin
	slotMiterLimit
	slotDash
	slotFillRule
	slotAntiAlias
	slotClearClip
	slotCount
)

// compressibleSlot returns the state slot written by cmd, or slotNone when
// cmd is a structural command (Save/Restore, clip set, drawing) that must
// delimit a compression run.
func compressibleSlot(cmd Command) stateSlot {
	switch cmd.(type) {
	case SetTransformCommand:
		return slotTransform
	case SetFillStyleCommand:
		return slotFillStyle
	case SetStrokeStyleCommand:
		return slotStrokeStyle
	case SetLineWidthCommand:
		return slotLineWidth
	case SetLineCapCommand:
		return slotLineCap
	case SetLineJoinCommand:
		return slotLineJoin
	case SetMiterLimitCommand:
		return slotMiterLimit
	case SetDashCommand:
		return slotDash
	case SetFillRuleCommand:
		return slotFillRule
	case SetAntiAliasCommand:
		return slotAntiAlias
	case ClearClipCommand:
		return slotClearClip
	default:
		return slotNone
	}
}

// matrixIsFinite reports whether every component of m is a finite number.
func matrixIsFinite(m Matrix) bool {
	for _, v := range [6]float64{m.A, m.B, m.C, m.D, m.E, m.F} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// commandCompressor consumes commands in a single linear pass. Runs of
// compressible state commands are buffered until the next barrier command;
// within each run only the last write per state slot survives.
type commandCompressor struct {
	out       []Command
	run       []Command
	lastIndex [slotCount]int // index into run, -1 when the slot is absent
	stats     CompressionStats
	depth     int
}

func newCommandCompressor(inputSize int) *commandCompressor {
	c := &commandCompressor{
		out: make([]Command, 0, inputSize),
		run: make([]Command, 0, 16),
	}
	for i := range c.lastIndex {
		c.lastIndex[i] = -1
	}
	return c
}

// ingest classifies cmd and either folds it into the current run or flushes
// the run and retains the barrier verbatim.
func (c *commandCompressor) ingest(cmd Command) {
	if cmd == nil || int(cmd.Type()) >= len(commandTypeNames) {
		// Unknown input is never removed; it also breaks any run.
		c.flushRun()
		c.out = append(c.out, cmd)
		c.stats.KeptUnknown++
		return
	}
	slot := compressibleSlot(cmd)
	if slot == slotNone {
		c.ingestBarrier(cmd)
		return
	}
	c.ingestState(cmd, slot)
}

// ingestBarrier retains a structural command and tracks Save/Restore nesting.
func (c *commandCompressor) ingestBarrier(cmd Command) {
	c.flushRun()
	c.out = append(c.out, cmd)
	c.stats.KeptBarrier++

	switch cmd.(type) {
	case SaveCommand:
		c.depth++
		if c.depth > c.stats.MaxStackDepth {
			c.stats.MaxStackDepth = c.depth
		}
	case RestoreCommand:
		if c.depth == 0 {
			c.stats.UnbalancedRestore++
			return
		}
		c.depth--
	}
}

// ingestState folds an absolute state-set command into the current run,
// retiring any earlier write to the same slot.
func (c *commandCompressor) ingestState(cmd Command, slot stateSlot) {
	if tc, ok := cmd.(SetTransformCommand); ok && !matrixIsFinite(tc.Matrix) {
		c.stats.NonFiniteMatrices++
	}

	idx := c.lastIndex[slot]
	if idx >= 0 {
		if reflect.DeepEqual(c.run[idx], cmd) {
			c.stats.RedundantCommands++
		} else {
			c.stats.SupersededCommands++
		}
		// Retire the earlier write in place; the replacement is appended at
		// the end so surviving commands stay ordered by their final write.
		c.run[idx] = nil
	}
	c.lastIndex[slot] = len(c.run)
	c.run = append(c.run, cmd)
}

// flushRun emits the surviving writes of the current run and resets it.
func (c *commandCompressor) flushRun() {
	for _, cmd := range c.run {
		if cmd == nil {
			continue
		}
		c.out = append(c.out, cmd)
		c.stats.KeptLastWrite++
	}
	c.run = c.run[:0]
	for i := range c.lastIndex {
		c.lastIndex[i] = -1
	}
}

// Compress returns a new [Recording] in which adjacent state commands with
// equivalent final effect are collapsed. Transform, style and clip-clear
// commands are merged only within runs of consecutive state commands;
// Save/Restore, clip-set commands, drawing commands and unknown commands
// delimit runs and are never removed or reordered across.
//
// The resource pool is shared with the input recording and is never modified,
// so every resource reference in a retained command stays valid and identical.
// Compression is deterministic: empty recordings, single commands and
// repeated identical states produce stable output, and compressing the
// result again is an idempotent no-op.
//
// When rec is nil or opts.Enabled is false, the input recording is returned
// unchanged with zero stats.
func Compress(rec *Recording, opts CompressOptions) (*Recording, CompressionStats) {
	if rec == nil || !opts.Enabled {
		return rec, CompressionStats{}
	}

	c := newCommandCompressor(len(rec.commands))
	for _, cmd := range rec.commands {
		c.ingest(cmd)
	}
	c.flushRun()

	// Trim the backing array to the exact output size so long-lived exported
	// recordings do not retain the input's larger allocation.
	commands := make([]Command, len(c.out))
	copy(commands, c.out)

	c.stats.OriginalCommands = len(rec.commands)
	c.stats.CompressedCommands = len(commands)
	c.stats.RemovedCommands = c.stats.OriginalCommands - c.stats.CompressedCommands

	compressed := &Recording{
		width:     rec.width,
		height:    rec.height,
		commands:  commands,
		resources: rec.resources,
	}
	return compressed, c.stats
}

// Compress compresses the recording's command stream. See the package-level
// [Compress] function for the merging rules and guarantees.
func (r *Recording) Compress(opts CompressOptions) (*Recording, CompressionStats) {
	return Compress(r, opts)
}
