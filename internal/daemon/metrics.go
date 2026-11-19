package daemon

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"immich-ferry/internal/engine"
)

type metrics struct {
	runs        *prometheus.CounterVec
	lastSuccess *prometheus.GaugeVec
	lastRun     *prometheus.GaugeVec
	duration    *prometheus.GaugeVec
	copied      *prometheus.CounterVec
	bytes       *prometheus.CounterVec
	unlinked    *prometheus.CounterVec
	metaUpdates *prometheus.CounterVec
	skipped     *prometheus.CounterVec
	itemErrors  *prometheus.CounterVec
	albumAssets *prometheus.GaugeVec
	pending     *prometheus.GaugeVec
	guard       *prometheus.GaugeVec
	snapshotOK  *prometheus.GaugeVec
	preflightOK *prometheus.GaugeVec

	discoveryOK      prometheus.Gauge
	discoveredPairs  prometheus.Gauge
	discoverySkipped prometheus.Gauge
	albumsCreated    *prometheus.CounterVec
	pairsTornDown    *prometheus.CounterVec
	assetsTrashed    *prometheus.CounterVec
	pairVecs         []interface {
		DeletePartialMatch(prometheus.Labels) int
	}
}

func newMetrics(reg prometheus.Registerer) *metrics {
	c := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "immich_ferry", Name: name, Help: help}, labels)
	}
	g := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "immich_ferry", Name: name, Help: help}, labels)
	}
	m := &metrics{
		runs:        c("runs_total", "Completed pair runs by result.", "pair", "result"),
		lastSuccess: g("last_success_timestamp_seconds", "Unix time of the last ok run, or partial run with no item errors.", "pair"),
		lastRun:     g("last_run_timestamp_seconds", "Unix time the last run finished, whatever its result.", "pair"),
		duration:    g("run_duration_seconds", "Duration of the last run in seconds.", "pair"),
		copied:      c("assets_copied_total", "Assets copied between instances.", "pair", "direction"),
		// DESIGN-DEVIATION: RunSummary only has BytesDown/BytesUp totals, so the label is kind=down|up instead of direction.
		bytes:       c("bytes_total", "Bytes downloaded (kind=down) and uploaded (kind=up).", "pair", "kind"),
		unlinked:    c("unlinked_total", "Assets unlinked from an album to propagate a removal.", "pair", "direction"),
		metaUpdates: c("metadata_updates_total", "Asset metadata updates written.", "pair", "direction"),
		skipped:     c("skipped_total", "Items skipped by reason.", "pair", "reason"),
		itemErrors:  c("item_errors_total", "Per-item errors by kind.", "pair", "kind"),
		albumAssets: g("album_assets", "Assets in the album at last snapshot.", "pair", "side"),
		pending:     g("pending", "Items still pending (e.g. awaiting metadata extraction).", "pair"),
		guard:       g("guard_tripped", "1 if the mass-removal guard tripped on this side.", "pair", "side"),
		snapshotOK:  g("snapshot_consistent", "1 if the snapshot was consistent.", "pair", "side"),
		preflightOK: g("preflight_ok", "1 if preflight succeeded for this side.", "side"),

		discoveryOK: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "immich_ferry", Name: "discovery_ok",
			Help: "1 if the last album discovery succeeded (only with discovery enabled)."}),
		discoveredPairs: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "immich_ferry", Name: "discovered_pairs",
			Help: "Album pairs found by the last successful discovery."}),
		discoverySkipped: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "immich_ferry", Name: "discovery_problems",
			Help: "Selected albums the last discovery could not pair or create (name conflicts, errors)."}),
		albumsCreated: c("albums_created_total", "Counterpart albums created by discovery.", "side"),
		pairsTornDown: c("pairs_torn_down_total",
			"Discovered pairs torn down because everyone their created album was shared with left it (side of that album).", "side"),
		assetsTrashed: c("assets_trashed_total", "Assets the teardown moved to the trash (SYNC_DISCOVERY_TEARDOWN_TRASH).", "side"),
	}
	reg.MustRegister(m.runs, m.lastSuccess, m.lastRun, m.duration, m.copied, m.bytes, m.unlinked, m.metaUpdates, m.skipped,
		m.itemErrors, m.albumAssets, m.pending, m.guard, m.snapshotOK, m.preflightOK,
		m.discoveryOK, m.discoveredPairs, m.discoverySkipped, m.albumsCreated, m.pairsTornDown, m.assetsTrashed)
	m.pairVecs = []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{m.runs, m.lastSuccess, m.lastRun, m.duration, m.copied, m.bytes, m.unlinked, m.metaUpdates, m.skipped,
		m.itemErrors, m.albumAssets, m.pending, m.guard, m.snapshotOK}
	return m
}

// forgetPair drops every series of a pair that is no longer synced.
func (m *metrics) forgetPair(pair string) {
	for _, v := range m.pairVecs {
		v.DeletePartialMatch(prometheus.Labels{"pair": pair})
	}
}

func (m *metrics) observeDiscovery(rep engine.DiscoveryReport, err error) {
	if err != nil {
		m.discoveryOK.Set(0)
		return
	}
	m.discoveryOK.Set(1)
	m.discoveredPairs.Set(float64(len(rep.Pairs)))
	m.discoverySkipped.Set(float64(len(rep.Skipped) + len(rep.Errors)))
	for _, c := range rep.Created {
		m.albumsCreated.WithLabelValues(c.Side).Inc()
	}
	for _, n := range rep.TornDown {
		m.pairsTornDown.WithLabelValues(n.Side).Inc()
	}
	for _, n := range rep.Trashed {
		m.assetsTrashed.WithLabelValues(n.Side).Add(float64(n.Assets))
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (m *metrics) setPreflight(a, b bool) {
	m.preflightOK.WithLabelValues("a").Set(b2f(a))
	m.preflightOK.WithLabelValues("b").Set(b2f(b))
}

func (m *metrics) perDir(v *prometheus.CounterVec, pair string, p engine.PerDir) {
	v.WithLabelValues(pair, "a_to_b").Add(float64(p.AtoB))
	v.WithLabelValues(pair, "b_to_a").Add(float64(p.BtoA))
}

func (m *metrics) observe(pair string, s engine.RunSummary, failed bool) {
	res := string(s.Result)
	if failed || res == "" {
		res = string(engine.ResultFailed)
	}
	m.runs.WithLabelValues(pair, res).Inc()
	fin := s.Finished
	if fin.IsZero() {
		fin = time.Now()
	}
	m.lastRun.WithLabelValues(pair).Set(float64(fin.Unix()))
	// A partial run in which every item failed is not a success.
	if res == string(engine.ResultOK) || (res == string(engine.ResultPartial) && s.ItemErrorCount == 0) {
		m.lastSuccess.WithLabelValues(pair).Set(float64(fin.Unix()))
	}
	if !s.Started.IsZero() && !s.Finished.IsZero() {
		m.duration.WithLabelValues(pair).Set(s.Finished.Sub(s.Started).Seconds())
	}
	m.perDir(m.copied, pair, s.Copied)
	m.perDir(m.unlinked, pair, s.Unlinked)
	m.perDir(m.metaUpdates, pair, s.MetaUpdates)
	m.bytes.WithLabelValues(pair, "down").Add(float64(s.BytesDown))
	m.bytes.WithLabelValues(pair, "up").Add(float64(s.BytesUp))
	for r, n := range s.Skipped {
		m.skipped.WithLabelValues(pair, string(r)).Add(float64(n))
	}
	for _, ie := range s.ItemErrors {
		m.itemErrors.WithLabelValues(pair, ie.Kind.String()).Inc()
	}
	if extra := s.ItemErrorCount - len(s.ItemErrors); extra > 0 {
		m.itemErrors.WithLabelValues(pair, "other").Add(float64(extra))
	}
	for i, side := range []string{"a", "b"} {
		if s.AlbumIDs[i] == "" {
			continue // snapshot phase never completed for this side: keep the previous gauges
		}
		m.albumAssets.WithLabelValues(pair, side).Set(float64(s.AlbumSizes[i]))
		m.guard.WithLabelValues(pair, side).Set(b2f(s.GuardTripped[i]))
		m.snapshotOK.WithLabelValues(pair, side).Set(b2f(s.SnapshotConsistent[i]))
	}
	m.pending.WithLabelValues(pair).Set(float64(s.Pending))
}
