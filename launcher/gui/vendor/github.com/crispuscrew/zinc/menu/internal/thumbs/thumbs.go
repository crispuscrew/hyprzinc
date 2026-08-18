// Package thumbs is an asynchronous, caching thumbnail store for the menu's grid layout: decoding a
// directory of multi-megabyte photos on the render path would freeze the overlay. Get returns a
// cached thumbnail or schedules a bounded background decode and returns nil; the frame loop polls
// Pending/TakeDirty. Decoding is pure Go, so this stays cgo-free and static.
package thumbs

import (
	"image"
	"sync"

	"github.com/crispuscrew/zinc/menu/internal/imgutil"
)

const (
	// maxThumbBytes caps how much of a source file is read. Wallpapers are large, so this is
	// far more generous than the icon cap, but still bounds a hostile or corrupt file.
	maxThumbBytes = 64 << 20
	// maxThumbDecoded is the memory one decode may allocate: an 8K photograph decodes to about 132 MiB.
	// A byte budget rather than a pixel count, because the same dimensions cost 8x more at 16 bits per
	// channel than as a paletted image.
	maxThumbDecoded = 160 << 20
	// maxInFlight bounds concurrent decodes so a big grid cannot spawn hundreds of goroutines
	// each holding a full decoded image. With the budget above it also sets the ceiling on the
	// store's peak: at most maxInFlight x maxThumbDecoded of decoded pixels at any moment.
	maxInFlight = 3
)

// Store decodes and caches thumbnails scaled to fit a fixed box, off the render path. It is
// safe for concurrent use: the render goroutine calls Get/Pending/TakeDirty while background
// goroutines decode.
type Store struct {
	boxW, boxH int

	mu       sync.Mutex
	cache    map[string]*image.RGBA // path -> thumbnail; a present nil means "decoded, no image"
	inflight map[string]bool        // paths currently decoding, so each is scheduled once
	dirty    bool                   // a decode has completed since the last TakeDirty

	sem chan struct{} // bounds concurrent decodes to cap(sem)
}

// New returns a store that fits thumbnails into boxW x boxH pixels.
func New(boxW, boxH int) *Store {
	return &Store{
		boxW:     boxW,
		boxH:     boxH,
		cache:    map[string]*image.RGBA{},
		inflight: map[string]bool{},
		sem:      make(chan struct{}, maxInFlight),
	}
}

// Get returns the thumbnail for path, or nil when it is not ready, scheduling a decode on the first
// call. A path that failed to decode caches nil and is never rescheduled. The bool reports whether
// the decode has been attempted, which distinguishes "still loading" from "loaded, but no image".
func (store *Store) Get(path string) (*image.RGBA, bool) {
	if path == "" || store == nil {
		return nil, true
	}
	store.mu.Lock()
	if thumb, done := store.cache[path]; done {
		store.mu.Unlock()
		return thumb, true
	}
	if store.inflight[path] {
		store.mu.Unlock()
		return nil, false
	}
	store.inflight[path] = true
	store.mu.Unlock()

	go store.decode(path)
	return nil, false
}

// decode reads, scales, and caches one thumbnail, blocking on the concurrency semaphore first
// so at most maxInFlight run at once.
func (store *Store) decode(path string) {
	store.sem <- struct{}{}
	defer func() { <-store.sem }()

	thumb := imgutil.Fit(imgutil.Decode(path, maxThumbBytes, maxThumbDecoded), store.boxW, store.boxH)

	store.mu.Lock()
	store.cache[path] = thumb // may be nil: records that we tried and there is nothing to draw
	delete(store.inflight, path)
	store.dirty = true
	store.mu.Unlock()
}

// Pending reports whether any decode is still in flight, so the caller knows to keep polling.
func (store *Store) Pending() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.inflight) > 0
}

// Active reports whether any thumbnail work is left to observe: a decode in flight, or a finished one
// not yet drawn. One atomic read, because decode publishes, clears inflight and sets dirty in a
// single critical section - querying separately lets a decode land in the gap and strand its result.
func (store *Store) Active() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.dirty || len(store.inflight) > 0
}

// TakeDirty reports whether a decode has completed since the last call, clearing the flag, and
// whether any is still in flight. Both under one lock, for the reason in Active.
func (store *Store) TakeDirty() (dirty, pending bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	dirty, store.dirty = store.dirty, false
	return dirty, len(store.inflight) > 0
}
