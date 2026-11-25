package serversched

import (
	"testing"
	"time"

	"motty/internal/configstore"
	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// testEngineOnRaw builds an Engine over a caller-supplied raw store, so
// tests can restart the engine (or open a second Store) over the same
// database. It mirrors testEngine's defaults.
func testEngineOnRaw(t *testing.T, raw *configstore.Store, fc *fakeCollector, opts func(*Config)) (*Engine, *serverassets.Store) {
	t.Helper()
	assets := serverassets.NewStore(raw)
	cfg := Config{
		Store:         raw,
		Assets:        assets,
		Collector:     fc,
		Dial:          stubDialOK,
		TickInterval:  50 * time.Millisecond,
		RoundTimeout:  5 * time.Second,
		RetryInterval: 10 * time.Millisecond,
		MaxRetries:    2,
	}
	if opts != nil {
		opts(&cfg)
	}
	return New(cfg), assets
}

func saveFastTemplate(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.store.SaveTemplate(servercollect.Template{ID: "fast", Name: "Fast", Intervals: map[servercollect.MetricKind]time.Duration{
		servercollect.MetricCPU: 10 * time.Second,
	}}); err != nil {
		t.Fatalf("SaveTemplate(fast): %v", err)
	}
}

// TestEngineTemplateAssignmentSurvivesRestart: assigning a template
// persists it, and a new Engine over the same database picks it up on
// Start. Assigning "default" leaves no record behind.
func TestEngineTemplateAssignmentSurvivesRestart(t *testing.T) {
	raw := openTestRaw(t)
	fc := newFakeCollector()
	e1, assets := testEngineOnRaw(t, raw, fc, nil)
	servers := createServers(t, assets, 1)
	saveFastTemplate(t, e1)
	if err := e1.SetServerTemplate(servers[0].ID, "fast"); err != nil {
		t.Fatalf("SetServerTemplate: %v", err)
	}
	if got := e1.TemplateFor(servers[0].ID); got.ID != "fast" {
		t.Fatalf("TemplateFor = %s, want fast", got.ID)
	}
	e1.Start()
	e1.Stop()

	e2, _ := testEngineOnRaw(t, raw, fc, nil)
	e2.Start()
	defer e2.Stop()
	if got := e2.TemplateFor(servers[0].ID); got.ID != "fast" {
		t.Fatalf("restarted TemplateFor = %s, want fast", got.ID)
	}
	if err := e2.SetServerTemplate(servers[0].ID, "default"); err != nil {
		t.Fatalf("SetServerTemplate default: %v", err)
	}
	assigns, err := e2.store.ListTemplateAssignments()
	if err != nil || len(assigns) != 0 {
		t.Fatalf("default assignment left records: %v err=%v", assigns, err)
	}
	if got := e2.TemplateFor(servers[0].ID); got.ID != "default" {
		t.Fatalf("TemplateFor after default = %s, want default", got.ID)
	}
}

// TestEngineSetServerTemplateWriteFailureKeepsMemory: when the assignment
// cannot be written, SetServerTemplate reports the error and the
// in-memory assignment is left untouched.
func TestEngineSetServerTemplateWriteFailureKeepsMemory(t *testing.T) {
	raw := openTestRaw(t)
	fc := newFakeCollector()
	e, assets := testEngineOnRaw(t, raw, fc, nil)
	servers := createServers(t, assets, 1)
	saveFastTemplate(t, e)
	_ = raw.Close() // every later store write fails
	if err := e.SetServerTemplate(servers[0].ID, "fast"); err == nil {
		t.Fatal("SetServerTemplate on a closed store succeeded, want an error")
	}
	if got := e.TemplateFor(servers[0].ID); got.ID != "default" {
		t.Fatalf("TemplateFor = %s after failed assign, want default", got.ID)
	}
	if _, ok := e.TemplateFallback(servers[0].ID); ok {
		t.Fatal("failed assign recorded a fallback it never made")
	}
}

// TestEngineStopFlushesBufferedValues: with a flush interval so long the
// ticker never fires, values stay off disk until Stop, which flushes
// them before returning.
func TestEngineStopFlushesBufferedValues(t *testing.T) {
	raw := openTestRaw(t)
	fc := newFakeCollector()
	e, assets := testEngineOnRaw(t, raw, fc, func(cfg *Config) { cfg.FlushInterval = time.Hour })
	servers := createServers(t, assets, 1)
	startAndSettle(t, e, fc, servers)
	fresh := NewStore(raw)
	if _, found, _ := fresh.Get(servers[0].ID); found {
		t.Fatal("buffered values reached disk before Stop")
	}
	e.Stop()
	latest, found, err := fresh.Get(servers[0].ID)
	if err != nil || !found {
		t.Fatalf("fresh Get after Stop: found=%v err=%v", found, err)
	}
	for _, kind := range servercollect.AllMetricKinds {
		if _, ok := latest.Get(kind); !ok {
			t.Errorf("kind %s missing from disk after Stop", kind)
		}
	}
}

// TestEnginePauseFlushesBufferedValues: Pause flushes what has been
// collected so far, so a sleep or lock loses nothing already buffered.
func TestEnginePauseFlushesBufferedValues(t *testing.T) {
	raw := openTestRaw(t)
	fc := newFakeCollector()
	e, assets := testEngineOnRaw(t, raw, fc, func(cfg *Config) { cfg.FlushInterval = time.Hour })
	servers := createServers(t, assets, 1)
	startAndSettle(t, e, fc, servers)
	fresh := NewStore(raw)
	if _, found, _ := fresh.Get(servers[0].ID); found {
		t.Fatal("buffered values reached disk before Pause")
	}
	e.Pause()
	defer e.Stop()
	latest, found, err := fresh.Get(servers[0].ID)
	if err != nil || !found {
		t.Fatalf("fresh Get after Pause: found=%v err=%v", found, err)
	}
	for _, kind := range servercollect.AllMetricKinds {
		if _, ok := latest.Get(kind); !ok {
			t.Errorf("kind %s missing from disk after Pause", kind)
		}
	}
}

// TestEngineFlushIntervalFlushes: with a short flush interval the
// flusher writes buffered values to disk on its own, without any Stop
// or Pause.
func TestEngineFlushIntervalFlushes(t *testing.T) {
	raw := openTestRaw(t)
	fc := newFakeCollector()
	e, assets := testEngineOnRaw(t, raw, fc, func(cfg *Config) { cfg.FlushInterval = 50 * time.Millisecond })
	servers := createServers(t, assets, 1)
	startAndSettle(t, e, fc, servers)
	defer e.Stop()
	fresh := NewStore(raw)
	waitFor(t, "flusher to write values to disk", 10*time.Second, func() bool {
		latest, found, err := fresh.Get(servers[0].ID)
		if err != nil || !found {
			return false
		}
		for _, kind := range servercollect.AllMetricKinds {
			if _, ok := latest.Get(kind); !ok {
				return false
			}
		}
		return true
	})
}
