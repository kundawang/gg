//go:build !nogpu

package gpu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gogpu/gg"
)

// Upload queue errors.
var (
	// ErrUploadQueueClosed is returned when submitting to a closed queue.
	ErrUploadQueueClosed = errors.New("wgpu: upload queue is closed")

	// ErrUploadCanceled is delivered to Done callbacks of tasks that were
	// canceled before they started.
	ErrUploadCanceled = errors.New("wgpu: upload canceled")
)

// UploadQueueMetrics holds atomic counters for queue activity.
type UploadQueueMetrics struct {
	// Submitted is the number of tasks accepted by Submit.
	Submitted atomic.Uint64

	// Completed is the number of tasks that finished successfully.
	Completed atomic.Uint64

	// Failed is the number of tasks whose upload returned an error.
	Failed atomic.Uint64

	// Canceled is the number of tasks canceled before starting.
	Canceled atomic.Uint64
}

// UploadQueueMetricsSnapshot is a point-in-time copy of UploadQueueMetrics.
type UploadQueueMetricsSnapshot struct {
	Submitted uint64
	Completed uint64
	Failed    uint64
	Canceled  uint64
}

// Snapshot copies the metrics for reporting.
func (m *UploadQueueMetrics) Snapshot() UploadQueueMetricsSnapshot {
	return UploadQueueMetricsSnapshot{
		Submitted: m.Submitted.Load(),
		Completed: m.Completed.Load(),
		Failed:    m.Failed.Load(),
		Canceled:  m.Canceled.Load(),
	}
}

// uploadTask is one queued upload.
type uploadTask struct {
	id     uint64
	tex    *GPUTexture
	x, y   int
	pixmap *gg.Pixmap
	done   func(error)

	// cancelReason, once non-nil, prevents the task from running and is
	// delivered to done: ErrUploadCanceled for Cancel, ErrDeviceLost for
	// NotifyDeviceLost.
	cancelReason atomic.Value // stores error
}

// TextureUploadQueue executes texture uploads asynchronously on worker
// goroutines. It exists so the render path can hand off atlas/texture
// uploads without blocking, while guaranteeing:
//
//   - Cancellation: Cancel(id) prevents a not-yet-started task from running;
//     its Done callback receives ErrUploadCanceled.
//   - Device loss: NotifyDeviceLost cancels all pending tasks with
//     ErrDeviceLost and poisons the shared staging pool so in-flight
//     buffers are never reused.
//   - Reference safety: the queue Retains each texture for the lifetime of
//     its task, so Close on the texture cannot free memory mid-upload.
//   - Failure isolation: a failed upload poisons its staging buffer (inside
//     GPUTexture.stageAndWriteLocked); the buffer is discarded, not reused.
//
// The queue is safe for concurrent use.
type TextureUploadQueue struct {
	ctx    context.Context
	cancel context.CancelFunc

	ch      chan *uploadTask
	wg      sync.WaitGroup
	closed  atomic.Bool
	nextID  atomic.Uint64
	pending sync.Map // uint64 → *uploadTask

	pool *StagingPool // shared pool poisoned on device loss (may be nil)

	// uploadFn performs the actual upload; replaceable for fault injection.
	uploadFn func(t *GPUTexture, x, y int, pm *gg.Pixmap) error

	Metrics UploadQueueMetrics
}

// NewTextureUploadQueue creates a queue with the given number of workers and
// channel capacity. The pool, if non-nil, is poisoned on NotifyDeviceLost.
func NewTextureUploadQueue(workers, capacity int, pool *StagingPool) *TextureUploadQueue {
	if workers <= 0 {
		workers = 1
	}
	if capacity <= 0 {
		capacity = 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &TextureUploadQueue{
		ctx:      ctx,
		cancel:   cancel,
		ch:       make(chan *uploadTask, capacity),
		pool:     pool,
		uploadFn: defaultUploadFn,
	}
	q.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go q.worker()
	}
	return q
}

// defaultUploadFn uploads a pixmap region synchronously.
func defaultUploadFn(t *GPUTexture, x, y int, pm *gg.Pixmap) error {
	return t.UploadRegion(x, y, pm)
}

// SetUploadFunc replaces the upload implementation. Intended for tests and
// fault injection; call before submitting work.
func (q *TextureUploadQueue) SetUploadFunc(fn func(t *GPUTexture, x, y int, pm *gg.Pixmap) error) {
	q.uploadFn = fn
}

// Submit enqueues an upload of pixmap to texture at origin (x, y).
// The texture is retained until the task finishes, so it is safe to Close
// the texture after Submit returns — resources stay alive until the upload
// completes. done, if non-nil, is called exactly once with the result.
//
// Returns the task ID (usable with Cancel) or an error if the queue is
// closed, the texture is released, or the queue is full.
func (q *TextureUploadQueue) Submit(tex *GPUTexture, x, y int, pixmap *gg.Pixmap, done func(error)) (uint64, error) {
	if q.closed.Load() {
		return 0, ErrUploadQueueClosed
	}
	if tex == nil {
		return 0, ErrNilTexture
	}
	if tex.IsReleased() {
		return 0, ErrTextureReleased
	}
	if pixmap == nil {
		return 0, ErrNilPixmap
	}

	task := &uploadTask{
		id:     q.nextID.Add(1),
		tex:    tex,
		x:      x,
		y:      y,
		pixmap: pixmap,
		done:   done,
	}
	tex.Retain()
	q.pending.Store(task.id, task)

	select {
	case q.ch <- task:
		q.Metrics.Submitted.Add(1)
		return task.id, nil
	case <-q.ctx.Done():
		q.pending.Delete(task.id)
		tex.Release()
		return 0, ErrUploadQueueClosed
	default:
		// Queue full: fail fast rather than block the render path.
		q.pending.Delete(task.id)
		tex.Release()
		return 0, fmt.Errorf("wgpu: upload queue full (capacity %d)", cap(q.ch))
	}
}

// Cancel marks a pending task as canceled. Returns true if the task was
// found. A task already running completes normally; cancellation only
// prevents not-yet-started work.
func (q *TextureUploadQueue) Cancel(id uint64) bool {
	v, ok := q.pending.Load(id)
	if !ok {
		return false
	}
	task := v.(*uploadTask)
	task.cancelReason.CompareAndSwap(nil, ErrUploadCanceled)
	return true
}

// NotifyDeviceLost cancels all pending tasks with ErrDeviceLost and poisons
// the shared staging pool so that in-flight staging buffers are discarded
// rather than reused. Call Reset on the pool after device recovery.
func (q *TextureUploadQueue) NotifyDeviceLost() {
	if q.pool != nil {
		q.pool.NotifyDeviceLost()
	}
	q.pending.Range(func(_, v any) bool {
		task := v.(*uploadTask)
		task.cancelReason.CompareAndSwap(nil, ErrDeviceLost)
		return true
	})
}

// worker drains the task channel until Close.
func (q *TextureUploadQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			// Drain remaining tasks as canceled.
			for {
				select {
				case task := <-q.ch:
					q.finishCanceled(task)
				default:
					return
				}
			}
		case task := <-q.ch:
			q.run(task)
		}
	}
}

// run executes one task, honoring cancellation.
func (q *TextureUploadQueue) run(task *uploadTask) {
	q.pending.Delete(task.id)
	defer task.tex.Release()

	if reason, ok := task.cancelReason.Load().(error); ok && reason != nil {
		q.Metrics.Canceled.Add(1)
		if task.done != nil {
			task.done(reason)
		}
		return
	}

	err := q.uploadFn(task.tex, task.x, task.y, task.pixmap)
	if err != nil {
		q.Metrics.Failed.Add(1)
	} else {
		q.Metrics.Completed.Add(1)
	}
	if task.done != nil {
		task.done(err)
	}
}

// finishCanceled completes a task that never ran due to queue shutdown.
func (q *TextureUploadQueue) finishCanceled(task *uploadTask) {
	q.pending.Delete(task.id)
	q.Metrics.Canceled.Add(1)
	task.tex.Release()
	if task.done != nil {
		task.done(ErrUploadCanceled)
	}
}

// Close stops accepting new tasks, cancels everything still queued, and
// waits for workers to finish. In-flight uploads run to completion.
// Close is idempotent.
func (q *TextureUploadQueue) Close() {
	if q.closed.Swap(true) {
		return
	}
	q.cancel()
	q.wg.Wait()
}
