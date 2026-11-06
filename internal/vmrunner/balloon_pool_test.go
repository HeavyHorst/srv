package vmrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type balloonGuestFixture struct {
	mu        sync.Mutex
	stats     firecrackerBalloonStats
	failPatch bool
	hangPatch bool
}

func (g *balloonGuestFixture) sample(actual, target, available int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats = firecrackerBalloonStats{ActualMiB: int64Ptr(actual), TargetMiB: int64Ptr(target), AvailableMemory: int64Ptr(available * miBBytes), TotalMemory: 4096 * miBBytes}
}

func (g *balloonGuestFixture) target() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return *g.stats.TargetMiB
}

type balloonPoolFixture struct {
	t      *testing.T
	s      *Server
	pool   string
	now    time.Time
	guests map[string]*balloonGuestFixture
}

func newBalloonPoolFixture(t *testing.T, names ...string) *balloonPoolFixture {
	t.Helper()
	oldRoot, oldCurrent := cgroupFSRoot, currentCgroupPath
	root, err := os.MkdirTemp("", "balloon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cgroupFSRoot, currentCgroupPath = oldRoot, oldCurrent; os.RemoveAll(root) })
	cgroupFSRoot = filepath.Join(root, "cg")
	currentCgroupPath = func() (string, error) { return "/runner", nil }
	f := &balloonPoolFixture{t: t, s: NewServer(nil, ServerConfig{InstancesDir: filepath.Join(root, "vm"), JailerBaseDir: filepath.Join(root, "jail")}),
		pool: filepath.Join(cgroupFSRoot, "runner", firecrackerPoolRootCgroupName, "p"), now: time.Now(), guests: make(map[string]*balloonGuestFixture)}
	for _, name := range names {
		for _, dir := range []string{filepath.Join(f.pool, name), filepath.Join(f.s.config.InstancesDir, name)} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		g := &balloonGuestFixture{}
		g.sample(0, 0, 2048)
		f.guests[name] = g
		path := filepath.Join(f.s.config.InstancesDir, name, "firecracker.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		h := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(g.stats)
				return
			}
			var body struct {
				Amount int64 `json:"amount_mib"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			g.stats.TargetMiB = int64Ptr(body.Amount)
			if g.hangPatch {
				<-r.Context().Done()
				return
			}
			if g.failPatch {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(204)
		})}
		go h.Serve(ln)
		t.Cleanup(func() { h.Close(); f.s.closeFirecrackerClient(path) })
	}
	f.write("memory.max", 16384)
	f.write("memory.current", 8192)
	return f
}

func (f *balloonPoolFixture) write(name string, mib int64) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.pool, name), []byte(fmt.Sprint(mib*miBBytes)), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *balloonPoolFixture) sweep() {
	f.t.Helper()
	f.s.reconcileBalloonsAt(context.Background(), f.now)
	f.now = f.now.Add(15 * time.Second)
}

func TestPoolBalloonCancelWithoutHeadroomStats(t *testing.T) {
	for _, fallen := range []bool{false, true} {
		t.Run(fmt.Sprint("fallen=", fallen), func(t *testing.T) {
			f := newBalloonPoolFixture(t, "stalled", "donor")
			f.write("memory.current", 13440)
			g := f.guests["stalled"]
			g.sample(384, 1536, 1024)
			g.stats.AvailableMemory = nil
			f.sweep()
			f.sweep()
			if got := f.guests["donor"].target(); got != 0 {
				t.Fatalf("pending inflation not accounted: donor=%d", got)
			}
			actual := int64(384)
			if fallen {
				actual = 256
				f.now = f.now.Add(-10 * time.Second)
				g.mu.Lock()
				g.stats.ActualMiB = int64Ptr(actual)
				g.mu.Unlock()
			}
			f.sweep()
			if got := g.target(); got != actual {
				t.Fatalf("uncancelled inflation=%d, want %d", got, actual)
			}
			f.sweep()
			if got := f.guests["donor"].target(); got == 0 {
				t.Fatal("cancelled inflation still blocks responsive donor")
			}
		})
	}
}

func TestPoolBalloonPressureAndGuestRescue(t *testing.T) {
	f := newBalloonPoolFixture(t, "idle", "busy")
	f.guests["idle"].sample(512, 512, 1536)
	f.guests["busy"].sample(0, 0, 520)
	f.sweep()
	f.sweep()
	if got := f.guests["idle"].target(); got != 512 {
		t.Fatalf("low-pressure target=%d, want hold 512", got)
	}
	f.write("memory.current", 13824)
	f.sweep()
	if got := f.guests["idle"].target(); got != 1024 {
		t.Fatalf("donor target=%d, want 1024", got)
	}
	if got := f.guests["busy"].target(); got != 0 {
		t.Fatalf("busy guest inflated to %d", got)
	}
	// No stacking of unfinished work, even though host usage has not fallen.
	f.sweep()
	if got := f.guests["idle"].target(); got != 1024 {
		t.Fatalf("pending target increased to %d", got)
	}
	f.guests["idle"].sample(1024, 1024, 1024)
	f.write("memory.current", 12800)
	f.sweep()
	f.sweep()
	if got := f.guests["idle"].target(); got != 1024 {
		t.Fatalf("successful reclaim undone: %d", got)
	}
	// The guest can get memory back without waiting for a low-pool watermark.
	f.guests["idle"].sample(1024, 1024, 128)
	f.write("memory.current", 13312)
	f.sweep()
	if got := f.guests["idle"].target(); got != 640 {
		t.Fatalf("rescue target=%d, want 640", got)
	}
	// No host budget: don't promise that memory can be returned safely.
	f.guests["idle"].sample(640, 640, 128)
	f.write("memory.current", 16000)
	f.sweep()
	f.sweep()
	if got := f.guests["idle"].target(); got != 640 {
		t.Fatalf("saturated pool granted memory: %d", got)
	}
}

func TestPoolBalloonSharedBudgetAndUnknownPatchOutcome(t *testing.T) {
	f := newBalloonPoolFixture(t, "a", "b")
	f.write("memory.current", 13440) // 1152 above the 75% aim.
	f.guests["a"].failPatch = true
	f.guests["b"].failPatch = true
	f.sweep()
	f.sweep()
	if total := f.guests["a"].target() + f.guests["b"].target(); total != 512 {
		t.Fatalf("combined target=%d, want 512", total)
	}
	f.sweep()
	// One pending command plus at most one new 512-MiB round, not 512 per VM.
	if total := f.guests["a"].target() + f.guests["b"].target(); total != 1024 {
		t.Fatalf("combined pending target=%d, want 1024", total)
	}
	f.now = f.now.Add(time.Minute)
	f.sweep()
	if total := f.guests["a"].target() + f.guests["b"].target(); total != 0 {
		t.Fatalf("stalled work not cancelled: %d", total)
	}
}

func TestPoolBalloonInvalidMetricsAndResize(t *testing.T) {
	f := newBalloonPoolFixture(t, "a")
	f.write("memory.current", 14000)
	f.sweep()
	if err := os.WriteFile(filepath.Join(f.pool, "memory.current"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 0 {
		t.Fatalf("invalid usage inflated to %d", got)
	}
	f.write("memory.current", 14000)
	f.write("memory.max", 32768)
	f.sweep()
	f.sweep()
	if got := f.guests["a"].target(); got != 0 {
		t.Fatalf("ignored pool growth: %d", got)
	}
	f.write("memory.max", 16384)
	f.sweep()
	if got := f.guests["a"].target(); got != 0 {
		t.Fatalf("did not observe after resize: %d", got)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 512 {
		t.Fatalf("did not reclaim after resize: %d", got)
	}
}

func TestPoolBalloonGuestDeflationAndRestart(t *testing.T) {
	f := newBalloonPoolFixture(t, "a")
	f.guests["a"].sample(512, 512, 2048)
	f.write("memory.current", 14000)
	f.sweep()
	f.guests["a"].sample(384, 512, 2048)
	f.sweep()
	if got := f.guests["a"].target(); got != 384 {
		t.Fatalf("fought guest deflation: %d", got)
	}
	// A fresh runner observes outstanding inflation before issuing more work.
	f.s = NewServer(nil, f.s.config)
	f.guests["a"].sample(384, 896, 2048)
	f.sweep()
	f.sweep()
	if got := f.guests["a"].target(); got != 896 {
		t.Fatalf("restart stacked pending work: %d", got)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 384 {
		t.Fatalf("restart failed to expire stalled work: %d", got)
	}
}

func TestPoolBalloonSharedReturnBudget(t *testing.T) {
	f := newBalloonPoolFixture(t, "a", "b")
	f.write("memory.current", 14409) // 336 MiB below the 90% ceiling.
	for _, g := range f.guests {
		g.sample(512, 512, 0)
	}
	f.sweep()
	f.sweep()
	if total := f.guests["a"].target() + f.guests["b"].target(); total != 704 {
		t.Fatalf("combined target=%d, want 704 (320 MiB returned)", total)
	}
	f.sweep()
	if total := f.guests["a"].target() + f.guests["b"].target(); total != 704 {
		t.Fatalf("spent pending return twice: total target=%d", total)
	}
}

func TestPoolBalloonThresholdAndSmallPool(t *testing.T) {
	for _, tc := range []struct {
		name              string
		limit, used, want int64
	}{
		{"below", 16000, 12799, 0},
		{"at", 16000, 12800, 0},
		{"above", 16000, 12801, 512},
		{"small pool", 512, 420, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBalloonPoolFixture(t, "a")
			f.write("memory.max", tc.limit)
			f.write("memory.current", tc.used)
			f.sweep()
			f.sweep()
			if got := f.guests["a"].target(); got != tc.want {
				t.Fatalf("target=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestPoolBalloonReplacementDoesNotInheritObservations(t *testing.T) {
	f := newBalloonPoolFixture(t, "a")
	f.write("memory.current", 14000)
	f.sweep()
	// The same instance name now has a different cgroup inode. Even if its
	// socket stayed at the same path, old timing must not authorize inflation.
	path := filepath.Join(f.pool, "a")
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + "-old"); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 0 {
		t.Fatalf("replacement inherited warm observations: %d", got)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 512 {
		t.Fatalf("replacement did not reconcile: %d", got)
	}
}

func TestPoolBalloonSmallPoolHoldsHeadroomAndReturnsShortfall(t *testing.T) {
	f := newBalloonPoolFixture(t, "a")
	f.write("memory.max", 1024)
	f.write("memory.current", 400)
	f.guests["a"].sample(51, 51, 512)
	f.sweep()
	f.sweep()
	if got := f.guests["a"].target(); got != 51 {
		t.Fatalf("returned memory without guest demand: %d", got)
	}
	f.guests["a"].sample(51, 51, 500)
	f.sweep()
	if got := f.guests["a"].target(); got != 39 {
		t.Fatalf("did not return 12-MiB shortfall: %d", got)
	}
}

func TestPoolBalloonRescueDoesNotCompeteWithReclaimBudget(t *testing.T) {
	f := newBalloonPoolFixture(t, "donor", "busy")
	f.write("memory.current", 13926)
	f.guests["busy"].sample(512, 512, 0)
	f.sweep()
	f.sweep()
	if got := f.guests["busy"].target(); got != 0 {
		t.Fatalf("donor starved rescue: %d", got)
	}
	if got := f.guests["donor"].target(); got != 512 {
		t.Fatalf("missing bounded donor reclaim: %d", got)
	}
}

func TestPoolBalloonUnknownPeerPreventsNewGrants(t *testing.T) {
	f := newBalloonPoolFixture(t, "busy", "unknown")
	f.guests["busy"].sample(512, 512, 0)
	f.guests["unknown"].mu.Lock()
	f.guests["unknown"].stats = firecrackerBalloonStats{}
	f.guests["unknown"].mu.Unlock()
	f.sweep()
	f.sweep()
	if got := f.guests["busy"].target(); got != 512 {
		t.Fatalf("granted memory with unknown outstanding peer work: %d", got)
	}
	f.guests["unknown"].sample(0, 0, 2048)
	f.sweep()
	f.sweep()
	if got := f.guests["busy"].target(); got != 0 {
		t.Fatalf("did not rescue after peer became known: %d", got)
	}
}

func TestPoolBalloonAcceptedPatchTimeout(t *testing.T) {
	f := newBalloonPoolFixture(t, "a")
	f.write("memory.current", 14000)
	f.guests["a"].hangPatch = true
	f.sweep()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	f.s.reconcileBalloonsAt(ctx, f.now)
	f.now = f.now.Add(15 * time.Second)
	if got := f.guests["a"].target(); got != 512 {
		t.Fatalf("timed-out request did not apply: %d", got)
	}
	f.sweep()
	if got := f.guests["a"].target(); got != 512 {
		t.Fatalf("stacked a new request after timeout: %d", got)
	}
}
