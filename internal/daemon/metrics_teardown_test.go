package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"immich-ferry/internal/engine"
)

// observeDiscovery: TornDown counts one per pair (side label), Trashed adds
// the asset count per side; a failed discovery adds nothing.
func TestMetricsTeardownCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	rep := engine.DiscoveryReport{
		Mode: engine.DiscoveryAll,
		TornDown: []engine.AlbumNote{
			{Pair: "p1", Side: "b", AlbumID: "x"},
			{Pair: "p2", Side: "b", AlbumID: "y"},
			{Pair: "p3", Side: "a", AlbumID: "z"},
		},
		Trashed: []engine.AlbumNote{
			{Pair: "p1", Side: "b", Assets: 3},
			{Pair: "p2", Side: "b", Assets: 4},
			{Pair: "p3", Side: "a", Assets: 5},
		},
	}
	m.observeDiscovery(rep, nil)
	want := map[string]float64{
		`immich_ferry_pairs_torn_down_total{side="a"}`: 1,
		`immich_ferry_pairs_torn_down_total{side="b"}`: 2,
		`immich_ferry_assets_trashed_total{side="a"}`:  5,
		`immich_ferry_assets_trashed_total{side="b"}`:  7,
		`immich_ferry_discovery_ok{}`:                  1,
	}
	check := func(want map[string]float64) {
		t.Helper()
		got := gather(t, reg)
		for k, w := range want {
			if g, ok := got[k]; !ok || g != w {
				t.Errorf("%s: got %v (present=%v) want %v", k, g, ok, w)
			}
		}
	}
	check(want)

	// Counters accumulate across passes; a teardown without trashing adds no trashed assets.
	m.observeDiscovery(engine.DiscoveryReport{TornDown: []engine.AlbumNote{{Pair: "p4", Side: "a"}}}, nil)
	want[`immich_ferry_pairs_torn_down_total{side="a"}`] = 2
	check(want)

	// A failed discovery never counts its (partial) report.
	m.observeDiscovery(rep, errors.New("boom"))
	want[`immich_ferry_discovery_ok{}`] = 0
	check(want)
}

// Through Once: a discovery report with teardowns reaches the registry.
func TestOnceDiscoveryTeardownMetrics(t *testing.T) {
	f := &fakeRunner{results: map[string]engine.RunSummary{"album-1": {Result: engine.ResultOK}}}
	f.pairs = func([]engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
		rep := engine.DiscoveryReport{Mode: engine.DiscoveryAll,
			Pairs:    []engine.DiscoveredPairInfo{{Pair: "album-1"}},
			TornDown: []engine.AlbumNote{{Pair: "album-2", Side: "b", AlbumID: "x"}},
			Trashed:  []engine.AlbumNote{{Pair: "album-2", Side: "b", Assets: 6}},
		}
		return []engine.Pair{{Name: "album-1"}}, rep, nil
	}
	d, reg, buf := newD(t, f)
	d.cfg.Engine.Discovery = engine.DiscoveryAll
	for i := 1; i <= 2; i++ {
		if code := d.Once(context.Background()); code != 0 {
			t.Fatalf("round %d code %d\n%s", i, code, buf)
		}
		got := gather(t, reg)
		if got[`immich_ferry_pairs_torn_down_total{side="b"}`] != float64(i) ||
			got[`immich_ferry_assets_trashed_total{side="b"}`] != float64(6*i) {
			t.Fatalf("round %d metrics: %v", i, got)
		}
		if _, ok := got[`immich_ferry_pairs_torn_down_total{side="a"}`]; ok {
			t.Errorf("unexpected side a series")
		}
	}
}
