package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"srv/internal/model"
)

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	})
	return s
}

func BenchmarkGetInstance(b *testing.B) {
	ctx := context.Background()
	s := newBenchStore(b)
	inst := testInstance("bench", model.StateStopped, time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC))
	if err := s.CreateInstance(ctx, inst); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetInstance(ctx, inst.Name); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListInstances(b *testing.B) {
	ctx := context.Background()
	s := newBenchStore(b)
	for i := 0; i < 100; i++ {
		inst := testInstance(fmt.Sprintf("bench-%03d", i), model.StateStopped, time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC))
		if err := s.CreateInstance(ctx, inst); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ListInstances(ctx, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdmitLifecycleRequest(b *testing.B) {
	ctx := context.Background()
	s := newBenchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request := LifecycleRequest{ID: fmt.Sprintf("request-%d", i), Actor: "owner", Command: "start", Name: "bench", Arguments: "[]"}
		admitted, err := s.AdmitLifecycleRequest(ctx, request)
		if err != nil || !admitted {
			b.Fatalf("admit: admitted=%v err=%v", admitted, err)
		}
	}
}
