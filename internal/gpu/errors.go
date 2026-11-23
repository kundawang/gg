//go:build !nogpu

package gpu

import (
	"errors"

	"github.com/gogpu/gg"
	"github.com/gogpu/wgpu"
)

// Package errors for wgpu backend.
var (
	// ErrNotInitialized is returned when operations are called before Init.
	ErrNotInitialized = errors.New("wgpu: backend not initialized")

	// ErrNoGPU is returned when no GPU adapter is available.
	ErrNoGPU = errors.New("wgpu: no GPU adapter available")

	// ErrDeviceLost is returned when the GPU device is lost.
	ErrDeviceLost = errors.New("wgpu: GPU device lost")

	// ErrNotImplemented is returned for stub operations not yet implemented.
	ErrNotImplemented = errors.New("wgpu: operation not implemented")

	// ErrInvalidDimensions is returned when width or height is invalid.
	ErrInvalidDimensions = errors.New("wgpu: invalid dimensions")

	// ErrNilTarget is returned when target pixmap is nil.
	ErrNilTarget = errors.New("wgpu: nil target pixmap")

	// ErrNilScene is returned when scene is nil.
	ErrNilScene = errors.New("wgpu: nil scene")
)

// ClassifyGPUError wraps err so callers can detect lifecycle failures with
// errors.Is against gg's sentinels:
//
//   - device loss → gg.ErrDeviceLost (dispatch must stop, accelerator is lost)
//   - closed backend / released resource → gg.ErrAcceleratorClosed
//
// Unrelated errors are returned unchanged. Wrapping is idempotent and preserves
// the original error in the chain, so existing callers keep working.
func ClassifyGPUError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gg.ErrDeviceLost) || errors.Is(err, gg.ErrAcceleratorClosed) {
		return err
	}
	switch {
	case errors.Is(err, wgpu.ErrDeviceLost),
		errors.Is(err, wgpu.ErrContextReleased),
		errors.Is(err, wgpu.ErrMapDeviceLost):
		return errors.Join(gg.ErrDeviceLost, err)
	case errors.Is(err, ErrNotInitialized):
		return errors.Join(gg.ErrAcceleratorClosed, err)
	default:
		return err
	}
}
