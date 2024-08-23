package viamrtsp

import (
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

type poolItem struct {
	frameWrapper *avFrameWrapper
	lastAccess   time.Time
}

type framePool struct {
	items          []poolItem
	maxAge         time.Duration
	preseededCount int

	putCount   int
	getCount   int
	cleanCount int
	newCount   int

	mu           sync.Mutex
	stopCleaning chan struct{}
	logger       logging.Logger
}

func newFramePool(maxAge, cleanupInterval time.Duration, preseededCount int, logger logging.Logger) *framePool {
	pool := &framePool{
		items:          make([]poolItem, 0, preseededCount),
		maxAge:         maxAge,
		preseededCount: preseededCount,
		stopCleaning:   make(chan struct{}),
		logger:         logger,
	}

	// Pre-seed the pool
	for i := 0; i < preseededCount; i++ {
		pool.items = append(pool.items, poolItem{
			frameWrapper: pool.new(),
			lastAccess:   time.Now(),
		})
		pool.newCount++
	}

	go pool.cleanupRoutine(cleanupInterval)

	return pool
}

func (p *framePool) new() *avFrameWrapper {
	p.logger.Debug("newFunc for pool was called!")
	avFrame, err := newAVFrameWrapper()
	if err != nil {
		p.logger.Errorf("Failed to allocate AVFrame in frame pool: %v", err)
		return nil
	}
	return avFrame
}

func (p *framePool) get() *avFrameWrapper {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.items) == 0 {
		p.newCount++
		return p.new()
	}

	item := p.items[0]
	p.items = p.items[1:]

	p.logger.Debugf("Item was gotten from the pool. Len now: %d", len(p.items))
	p.getCount++
	return item.frameWrapper
}

func (p *framePool) put(frame *avFrameWrapper) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.items = append(p.items, poolItem{frameWrapper: frame, lastAccess: time.Now()})
	p.logger.Debugf("Item was put in pool. Len now: %d", len(p.items))
	p.putCount++
}

func (p *framePool) tryPut(frame *avFrameWrapper) {
	frame.mu.Lock()
	defer frame.mu.Unlock()
	p.logger.Debug("Trying to safely put frame back into pool.")

	if !frame.isFreed || frame.isBeingServed {
		p.put(frame)
	} else {
		p.logger.Debug("Frame was already freed (%t) or is currently being served (%t). Cannot put.", frame.isFreed, frame.isBeingServed)
	}
}

func (p *framePool) cleanupRoutine(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanup()
		case <-p.stopCleaning:
			return
		}
	}
}

func (p *framePool) cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.items) <= p.preseededCount {
		return
	}

	now := time.Now()
	updatedItems := make([]poolItem, 0, len(p.items))
	for _, item := range p.items {
		if now.Sub(item.lastAccess) < p.maxAge {
			item.frameWrapper.free()
			updatedItems = append(updatedItems, item)
			p.cleanCount++
		}
	}

	p.logger.Debugf("Post cleanup() num old items: %d", len(p.items))
	p.logger.Debugf("Post cleanup() num new items: %d", len(updatedItems))
	p.items = updatedItems
}

func (p *framePool) close() {
	close(p.stopCleaning)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Free remaining pool item frames
	p.logger.Debugf("Num pool items remaining at close: %d", len(p.items))
	for _, item := range p.items {
		item.frameWrapper.free()
	}

	// Clear the slice to release references
	p.items = nil

	// Report stats
	p.logger.Debugf("getCount: %d", p.getCount)
	p.logger.Debugf("putCount: %d", p.putCount)
	p.logger.Debugf("newCount: %d", p.newCount)
	p.logger.Debugf("cleanCount: %d", p.cleanCount)
}