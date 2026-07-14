package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func BenchmarkInitialAuthLoad1144(b *testing.B) {
	authDir := b.TempDir()
	for i := 0; i < 1144; i++ {
		payload := []byte(fmt.Sprintf(`{"type":"xai","access_token":"token-%d"}`, i))
		if errWrite := os.WriteFile(filepath.Join(authDir, fmt.Sprintf("xai-%04d.json", i)), payload, 0o600); errWrite != nil {
			b.Fatal(errWrite)
		}
	}
	for _, workers := range []int{1, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				var reads atomic.Int64
				originalRead := readInitialAuthFile
				readInitialAuthFile = func(path string) ([]byte, error) {
					reads.Add(1)
					return os.ReadFile(path)
				}
				queue := make(chan AuthUpdateBatch, 64)
				w := newInitialLoadTestWatcher(b, authDir, queue)
				done := w.StartInitialAuthLoad(context.Background(), workers)
				finished := false
				for !finished {
					select {
					case batch := <-queue:
						results := make([]AuthUpdateResult, 0, len(batch.Updates))
						for _, update := range batch.Updates {
							results = append(results, AuthUpdateResult{ID: update.ID, Loaded: true})
						}
						if batch.Result != nil {
							batch.Result <- results
						}
					case <-done:
						finished = true
					}
				}
				readInitialAuthFile = originalRead
				_ = w.Stop()
				if got := reads.Load(); got != 1144 {
					b.Fatalf("payload reads = %d, want 1144", got)
				}
			}
		})
	}
}
