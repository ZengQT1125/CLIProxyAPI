package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	cooldownPersistDebounce   = 100 * time.Millisecond
	cooldownPersistRetryDelay = time.Second
)

type cooldownStatePersister struct {
	apply      func(context.Context, []cooldownStatePersistEntry) error
	mu         sync.Mutex
	dirty      map[string]uint64
	running    bool
	flushToken chan struct{}
}

type cooldownStatePersistEntry struct {
	authID       string
	storeVersion uint64
}

func newCooldownStatePersister(apply func(context.Context, []cooldownStatePersistEntry) error) *cooldownStatePersister {
	flushToken := make(chan struct{}, 1)
	flushToken <- struct{}{}
	return &cooldownStatePersister{
		apply:      apply,
		dirty:      make(map[string]uint64),
		flushToken: flushToken,
	}
}

func (p *cooldownStatePersister) mark(storeVersion uint64, authIDs ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID != "" {
			if dirtyVersion, ok := p.dirty[authID]; !ok || storeVersion > dirtyVersion {
				p.dirty[authID] = storeVersion
			}
		}
	}
	if len(p.dirty) == 0 || p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	go p.run()
}

func (p *cooldownStatePersister) run() {
	delay := cooldownPersistDebounce
	for {
		timer := time.NewTimer(delay)
		<-timer.C

		errFlush := p.flush(context.Background())
		if errFlush != nil {
			log.Warnf("failed to persist cooldown state: %v", errFlush)
			delay = cooldownPersistRetryDelay
		} else {
			delay = cooldownPersistDebounce
		}

		p.mu.Lock()
		if len(p.dirty) == 0 {
			p.running = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *cooldownStatePersister) flush(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.flushToken:
		}

		entries := p.drain()
		if len(entries) == 0 {
			p.flushToken <- struct{}{}
			return nil
		}
		errApply := p.apply(ctx, entries)
		if errApply != nil {
			p.requeue(entries)
			p.flushToken <- struct{}{}
			return errApply
		}
		p.flushToken <- struct{}{}
	}
}

func (p *cooldownStatePersister) drain() []cooldownStatePersistEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dirty) == 0 {
		return nil
	}
	entries := make([]cooldownStatePersistEntry, 0, len(p.dirty))
	for authID, storeVersion := range p.dirty {
		entries = append(entries, cooldownStatePersistEntry{authID: authID, storeVersion: storeVersion})
		delete(p.dirty, authID)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].authID < entries[j].authID
	})
	return entries
}

func (p *cooldownStatePersister) requeue(entries []cooldownStatePersistEntry) {
	p.mu.Lock()
	for _, entry := range entries {
		if entry.authID != "" {
			if dirtyVersion, ok := p.dirty[entry.authID]; !ok || entry.storeVersion > dirtyVersion {
				p.dirty[entry.authID] = entry.storeVersion
			}
		}
	}
	p.mu.Unlock()
}
