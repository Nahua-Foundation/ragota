package obs

import (
	"errors"
	"sync"
	"testing"
)

func find(t *testing.T, snap []Metric, name string) Metric {
	t.Helper()
	for _, m := range snap {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q not found in snapshot %+v", name, snap)
	return Metric{}
}

func TestConcurrentIncAndRecord(t *testing.T) {
	const goroutines = 16
	const perGoroutine = 1000

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				Inc("test_conc_total", 1)
				RecordDuration("test_conc_seconds", 0.5)
			}
		}()
	}
	wg.Wait()

	snap := Snapshot()

	c := find(t, snap, "test_conc_total")
	if c.Type != TypeCounter {
		t.Errorf("type = %q, want counter", c.Type)
	}
	if want := int64(goroutines * perGoroutine); c.Value != want {
		t.Errorf("counter value = %d, want %d", c.Value, want)
	}

	s := find(t, snap, "test_conc_seconds")
	if s.Type != TypeSummary {
		t.Errorf("type = %q, want summary", s.Type)
	}
	if want := int64(goroutines * perGoroutine); s.Count != want {
		t.Errorf("summary count = %d, want %d", s.Count, want)
	}
	if want := float64(goroutines*perGoroutine) * 0.5; s.Sum != want {
		t.Errorf("summary sum = %f, want %f", s.Sum, want)
	}
}

func TestSnapshotSorted(t *testing.T) {
	Inc("test_sorted_b", 1)
	Inc("test_sorted_a", 2)
	RecordDuration("test_sorted_c", 1.25)

	snap := Snapshot()
	for i := 1; i < len(snap); i++ {
		if snap[i-1].Name >= snap[i].Name {
			t.Fatalf("snapshot not sorted: %q before %q", snap[i-1].Name, snap[i].Name)
		}
	}

	a := find(t, snap, "test_sorted_a")
	if a.Value != 2 {
		t.Errorf("test_sorted_a = %d, want 2", a.Value)
	}
	c := find(t, snap, "test_sorted_c")
	if c.Count != 1 || c.Sum != 1.25 {
		t.Errorf("test_sorted_c = {count:%d sum:%f}, want {1 1.25}", c.Count, c.Sum)
	}
}

func TestIncByIgnoresNonPositive(t *testing.T) {
	Reset()
	IncBy("test_incby_total", 0)
	IncBy("test_incby_total", -3)
	if v := Value("test_incby_total"); v != 0 {
		t.Fatalf("counter = %d, want 0", v)
	}
	IncBy("test_incby_total", 4)
	if v := Value("test_incby_total"); v != 4 {
		t.Errorf("counter = %d, want 4", v)
	}
}

func TestObserveRecordsDurationAndFailure(t *testing.T) {
	Reset()

	if err := Observe("test_obs_seconds", "test_obs_failures", func() error { return nil }); err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if v := Value("test_obs_failures"); v != 0 {
		t.Errorf("failure counter = %d, want 0", v)
	}

	wantErr := errors.New("boom")
	if err := Observe("test_obs_seconds", "test_obs_failures", func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("Observe() error = %v, want %v", err, wantErr)
	}
	if v := Value("test_obs_failures"); v != 1 {
		t.Errorf("failure counter = %d, want 1", v)
	}
	if count, _ := SummaryOf("test_obs_seconds"); count != 2 {
		t.Errorf("summary count = %d, want 2", count)
	}
}

func TestResetClearsRegistry(t *testing.T) {
	Inc("test_reset_total", 5)
	RecordDuration("test_reset_seconds", 1)
	Reset()

	if v := Value("test_reset_total"); v != 0 {
		t.Errorf("counter = %d, want 0 after Reset", v)
	}
	if count, sum := SummaryOf("test_reset_seconds"); count != 0 || sum != 0 {
		t.Errorf("summary = {%d %f}, want zero after Reset", count, sum)
	}
	if len(Snapshot()) != 0 {
		t.Errorf("snapshot = %v, want empty after Reset", Snapshot())
	}
}
