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
	apply      func(context.Context, []string) error
	mu         sync.Mutex
	dirty      map[string]struct{}
	running    bool
	flushToken chan struct{}
}

func newCooldownStatePersister(apply func(context.Context, []string) error) *cooldownStatePersister {
	flushToken := make(chan struct{}, 1)
	flushToken <- struct{}{}
	return &cooldownStatePersister{
		apply:      apply,
		dirty:      make(map[string]struct{}),
		flushToken: flushToken,
	}
}

func (p *cooldownStatePersister) mark(authIDs ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID != "" {
			p.dirty[authID] = struct{}{}
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

		authIDs := p.drain()
		if len(authIDs) == 0 {
			p.flushToken <- struct{}{}
			return nil
		}
		errApply := p.apply(ctx, authIDs)
		if errApply != nil {
			p.requeue(authIDs)
			p.flushToken <- struct{}{}
			return errApply
		}
		p.flushToken <- struct{}{}
	}
}

func (p *cooldownStatePersister) drain() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dirty) == 0 {
		return nil
	}
	authIDs := make([]string, 0, len(p.dirty))
	for authID := range p.dirty {
		authIDs = append(authIDs, authID)
		delete(p.dirty, authID)
	}
	sort.Strings(authIDs)
	return authIDs
}

func (p *cooldownStatePersister) requeue(authIDs []string) {
	p.mu.Lock()
	for _, authID := range authIDs {
		if authID != "" {
			p.dirty[authID] = struct{}{}
		}
	}
	p.mu.Unlock()
}
