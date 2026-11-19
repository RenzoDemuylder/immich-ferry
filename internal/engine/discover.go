package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// DiscoveryMode selects how album pairs are found.
type DiscoveryMode string

// Discovery modes.
const (
	// DiscoveryOff uses the configured pairs.
	DiscoveryOff DiscoveryMode = ""
	// DiscoveryAll syncs every album the user owns or may edit.
	DiscoveryAll DiscoveryMode = "all"
	// DiscoveryMarker syncs albums whose description contains Options.DiscoveryMarker
	// (on either side).
	DiscoveryMarker DiscoveryMode = "marker"
)

// DiscoveredPairPrefix prefixes the names of discovered pairs ("album-<album id on A>").
const DiscoveredPairPrefix = "album-"

// DiscoveryReport describes one discovery pass.
type DiscoveryReport struct {
	Mode DiscoveryMode `json:"mode"`
	// Pairs are the pairs synced this cycle.
	Pairs []DiscoveredPairInfo `json:"pairs"`
	// Created lists albums created as counterparts this cycle.
	Created []AlbumNote `json:"created,omitempty"`
	// WouldCreate lists counterparts a mutating run would create (dry run / validate).
	WouldCreate []AlbumNote `json:"wouldCreate,omitempty"`
	// Unpaired lists pairs that stopped being synced this cycle.
	Unpaired []AlbumNote `json:"unpaired,omitempty"`
	// Skipped lists selected albums that cannot be paired (name conflicts, deleted counterpart, ...).
	Skipped []AlbumNote `json:"skipped,omitempty"`
	// Errors lists per-album failures (e.g. a failed album creation); the rest of the pass continues.
	Errors []AlbumNote `json:"errors,omitempty"`
	// TornDown lists pairs whose created album everyone left and that were torn down
	// (original album left, created album deleted, unpaired) this cycle.
	TornDown []AlbumNote `json:"tornDown,omitempty"`
	// WouldTeardown lists pairs a mutating run would tear down (dry run / validate).
	WouldTeardown []AlbumNote `json:"wouldTeardown,omitempty"`
	// Trashed lists assets moved to the trash by a teardown (Assets = count).
	Trashed []AlbumNote `json:"trashed,omitempty"`
}

// DiscoveredPairInfo is one active discovered pair.
type DiscoveredPairInfo struct {
	Pair     string    `json:"pair"`
	AlbumIDs [2]string `json:"albumIds"`
	Name     string    `json:"name"` // album name on A
}

// AlbumNote is a discovery event about one album or pair.
type AlbumNote struct {
	Pair    string `json:"pair,omitempty"`
	Side    string `json:"side,omitempty"`
	AlbumID string `json:"albumId,omitempty"`
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Assets  int    `json:"assets,omitempty"`
}

// Problems reports whether the pass skipped albums or hit errors.
func (r DiscoveryReport) Problems() bool { return len(r.Skipped) > 0 || len(r.Errors) > 0 }

// Pairs returns the pairs to sync this cycle. With discovery off it returns static.
// Otherwise it lists the albums on both sides, keeps the pinned pairs from the discovery
// state, pairs newly selected albums with an album of the same name on the other side,
// and creates the counterpart when there is none. Dry runs and read-only stores never
// create albums or write state. An error means discovery failed as a whole (listing or
// state IO); per-album problems are only reported.
func (e *Engine) Pairs(ctx context.Context, static []Pair) ([]Pair, DiscoveryReport, error) {
	rep := DiscoveryReport{Mode: e.o.Discovery}
	if e.o.Discovery == DiscoveryOff {
		return static, rep, nil
	}
	e.mu.Lock()
	done := e.pfDone
	e.mu.Unlock()
	if !done {
		if err := e.Preflight(ctx); err != nil {
			return nil, rep, err
		}
	}
	e.mu.Lock()
	users := e.userIDs
	e.mu.Unlock()
	d := &discovery{e: e, ctx: ctx, users: users, rep: &rep,
		mutating: !e.o.DryRun && !e.st.ReadOnly()}
	return d.run()
}

type discovery struct {
	e        *Engine
	ctx      context.Context
	users    [2]string
	rep      *DiscoveryReport
	mutating bool

	st     *state.Discovery
	albums [2][]immich.Album
	byID   [2]map[string]immich.Album
	pinned [2]map[string]bool
	tomb   [2]map[string]bool
	// torn: albums tombstoned by a teardown; never paired again, not even by name.
	torn  [2]map[string]bool
	pairs []Pair

	// share caches the resolved Options.DiscoveryShare per side for this pass.
	share     [2][]immich.AlbumUser
	shareErr  [2]error
	shareDone [2]bool
}

func (d *discovery) run() ([]Pair, DiscoveryReport, error) {
	e := d.e
	st, status, err := e.st.LoadDiscovery(d.users)
	if err != nil {
		return nil, *d.rep, err
	}
	if status == state.CorruptMovedAside || status == state.FingerprintReset {
		e.log.Warn("discovery state discarded; albums are re-matched by name", "op", "discovery", "status", loadStatusName(status))
	}
	d.st = st
	for _, s := range []Side{A, B} {
		l, err := e.cl[s].ListAlbums(d.ctx)
		if err != nil {
			return nil, *d.rep, fmt.Errorf("list albums on side %s: %w", s, err)
		}
		sort.Slice(l, func(i, j int) bool {
			if l[i].Name != l[j].Name {
				return l[i].Name < l[j].Name
			}
			return l[i].ID < l[j].ID
		})
		d.albums[s] = l
		d.byID[s] = make(map[string]immich.Album, len(l))
		for _, al := range l {
			d.byID[s][al.ID] = al
		}
		d.pinned[s] = map[string]bool{}
		d.tomb[s] = map[string]bool{}
		d.torn[s] = map[string]bool{}
	}
	for _, t := range d.st.Tombstones {
		if t.Side <= 1 {
			d.tomb[t.Side][t.AlbumID] = true
			d.torn[t.Side][t.AlbumID] = t.Reason == teardownReason
		}
	}
	if err := d.checkPinned(); err != nil {
		return nil, *d.rep, err
	}
	if err := d.gcTombstones(); err != nil {
		return nil, *d.rep, err
	}
	for _, s := range []Side{A, B} {
		for _, al := range d.albums[s] {
			if err := d.ctx.Err(); err != nil {
				return nil, *d.rep, err
			}
			if d.pinned[s][al.ID] || !d.editable(s, al) || !d.selected(al) {
				continue
			}
			if d.leftover(s, al) {
				d.note(&d.rep.Skipped, AlbumNote{Side: s.String(), AlbumID: al.ID, Name: al.Name,
					Reason: "owned by the dedicated sync user and nobody else is a member (everyone left?): not synced; delete it or share it again"})
				continue
			}
			if d.torn[s][al.ID] {
				d.note(&d.rep.Skipped, AlbumNote{Side: s.String(), AlbumID: al.ID, Name: al.Name,
					Reason: "its pair was torn down (" + teardownReason + "); delete or rename the album to clear this"})
				continue
			}
			d.pairNew(s, al)
		}
	}
	if d.mutating {
		if err := e.st.SaveDiscovery(d.st); err != nil {
			return nil, *d.rep, err
		}
	}
	sort.Slice(d.pairs, func(i, j int) bool { return d.pairs[i].Name < d.pairs[j].Name })
	sort.Slice(d.rep.Pairs, func(i, j int) bool { return d.rep.Pairs[i].Pair < d.rep.Pairs[j].Pair })
	e.log.Info("discovery finished", "op", "discovery", "mode", string(e.o.Discovery), "pairs", len(d.pairs),
		"created", len(d.rep.Created), "would_create", len(d.rep.WouldCreate), "unpaired", len(d.rep.Unpaired),
		"skipped", len(d.rep.Skipped), "errors", len(d.rep.Errors))
	return d.pairs, *d.rep, nil
}

func (d *discovery) editable(s Side, al immich.Album) bool { return canEdit(roleOf(al, d.users[s])) }

func (d *discovery) selected(al immich.Album) bool {
	switch d.e.o.Discovery {
	case DiscoveryAll:
		return true
	case DiscoveryMarker:
		return d.e.o.DiscoveryMarker != "" && strings.Contains(al.Description, d.e.o.DiscoveryMarker)
	}
	return false
}

// lookup returns the album from the listing, or confirms its absence with a GET (a
// listing glitch must never unpair). gone is true only for a definite not-found.
func (d *discovery) lookup(s Side, id string) (al immich.Album, gone bool, err error) {
	if al, ok := d.byID[s][id]; ok {
		return al, false, nil
	}
	al, err = d.e.cl[s].GetAlbum(d.ctx, id)
	if err == nil {
		return al, false, nil
	}
	if immich.KindOf(err) == immich.KindNotFound && d.ctx.Err() == nil {
		return immich.Album{}, true, nil
	}
	return immich.Album{}, false, err
}

// checkPinned keeps, skips or unpairs each stored pair.
func (d *discovery) checkPinned() error {
	kept := d.st.Pairs[:0:0]
	for i := range d.st.Pairs {
		p := &d.st.Pairs[i]
		if p.Teardown != nil && p.Created != nil && p.Created.Side <= 1 {
			d.pinned[A][p.AlbumIDs[A]], d.pinned[B][p.AlbumIDs[B]] = true, true
			if d.teardown(p) {
				kept = append(kept, *p)
			}
			if err := d.ctx.Err(); err != nil {
				return err
			}
			continue
		}
		var als [2]immich.Album
		var gone [2]bool
		failed := false
		for _, s := range []Side{A, B} {
			d.pinned[s][p.AlbumIDs[s]] = true
			al, g, err := d.lookup(s, p.AlbumIDs[s])
			if err != nil {
				if d.ctx.Err() != nil {
					return d.ctx.Err()
				}
				failed = true
				d.note(&d.rep.Errors, AlbumNote{Pair: p.Name, Side: s.String(), AlbumID: p.AlbumIDs[s],
					Reason: "cannot read album: " + err.Error()})
				continue
			}
			als[s], gone[s] = al, g
		}
		if failed { // unknown: keep the pair, do not sync it this cycle
			kept = append(kept, *p)
			continue
		}
		reason, tombstone := "", false
		for _, s := range []Side{A, B} {
			switch {
			case gone[s]:
				reason, tombstone = fmt.Sprintf("album deleted or not accessible on side %s", s), true
			case !d.editable(s, als[s]):
				reason, tombstone = fmt.Sprintf("no longer owner or editor of the album on side %s", s), true
			}
			if reason != "" {
				break
			}
		}
		if reason == "" && !d.selected(als[A]) && !d.selected(als[B]) {
			reason = "album no longer selected"
		}
		if reason == "" {
			d.backfill(p, als)
			yes, failed := d.shouldTeardown(p, als)
			switch {
			case failed:
				kept = append(kept, *p)
			case yes:
				if d.teardown(p) {
					kept = append(kept, *p)
				}
				if err := d.ctx.Err(); err != nil {
					return err
				}
			default:
				kept = append(kept, *p)
				d.activate(p.Name, p.AlbumIDs, als[A].Name)
			}
			continue
		}
		d.unpair(*p, reason)
		if tombstone {
			for _, s := range []Side{A, B} {
				if !gone[s] {
					d.tombstone(s, p.AlbumIDs[s], reason)
				}
			}
		}
	}
	d.st.Pairs = kept
	return nil
}

// unpair stops syncing p: its pair state is moved aside so a later re-pairing starts
// fresh (union, never removes). Albums and assets are left untouched.
func (d *discovery) unpair(p state.DiscoveredPair, reason string) {
	d.note(&d.rep.Unpaired, AlbumNote{Pair: p.Name, Reason: reason})
	d.e.log.Warn("album pair no longer synced", "op", "discovery", "pair", p.Name, "reason", reason,
		"album_a", p.AlbumIDs[A], "album_b", p.AlbumIDs[B])
	if !d.mutating {
		return
	}
	if err := d.e.st.Reset(p.Name); err != nil {
		d.note(&d.rep.Errors, AlbumNote{Pair: p.Name, Reason: "reset pair state: " + err.Error()})
	}
	d.e.releasePair(p.Name)
}

// gcTombstones drops tombstones of albums that no longer exist.
func (d *discovery) gcTombstones() error {
	kept := d.st.Tombstones[:0:0]
	for _, t := range d.st.Tombstones {
		if t.Side > 1 {
			continue
		}
		s := Side(t.Side)
		_, gone, err := d.lookup(s, t.AlbumID)
		if err != nil && d.ctx.Err() != nil {
			return d.ctx.Err()
		}
		if gone {
			delete(d.tomb[s], t.AlbumID)
			continue
		}
		kept = append(kept, t)
	}
	d.st.Tombstones = kept
	return nil
}

// pairNew pairs the selected, unpinned album al on side s with the album of the same
// name on the other side, or creates that album. Anything ambiguous is skipped.
func (d *discovery) pairNew(s Side, al immich.Album) {
	o := 1 - s
	skip := func(reason string) {
		d.note(&d.rep.Skipped, AlbumNote{Side: s.String(), AlbumID: al.ID, Name: al.Name, Reason: reason})
	}
	if al.Name == "" {
		skip("album has no name")
		return
	}
	for _, x := range d.albums[s] {
		if x.ID != al.ID && x.Name == al.Name && !d.pinned[s][x.ID] && d.editable(s, x) {
			skip(fmt.Sprintf("several albums named %q on side %s", al.Name, s))
			return
		}
	}
	var free []immich.Album
	blocked := 0
	for _, x := range d.albums[o] {
		if x.Name != al.Name {
			continue
		}
		if d.pinned[o][x.ID] || !d.editable(o, x) || d.torn[o][x.ID] || d.leftover(o, x) {
			blocked++
		} else {
			free = append(free, x)
		}
	}
	switch {
	case len(free) > 1:
		skip(fmt.Sprintf("several albums named %q on side %s", al.Name, o))
		return
	case blocked > 0:
		skip(fmt.Sprintf("an album named %q on side %s is already paired or not editable", al.Name, o))
		return
	case len(free) == 1:
		d.pin(s, al, free[0])
		return
	}
	if d.tomb[s][al.ID] {
		skip("its counterpart was deleted; not creating a new one (create an album with the same name to resume)")
		return
	}
	note := AlbumNote{Side: o.String(), Name: al.Name, Reason: fmt.Sprintf("counterpart of album %s on side %s", al.ID, s)}
	users, err := d.shareUsers(o)
	if err != nil {
		// Never create an album that cannot be shared as configured: report and retry next cycle.
		note.Reason = "resolve users to share the new album with: " + err.Error()
		d.note(&d.rep.Errors, note)
		d.e.log.Error("cannot resolve users to share a new album with; album not created",
			"op", "create_album", "side", o.String(), "kind", immich.KindOf(err).String(), "err", err)
		return
	}
	if !d.mutating {
		d.note(&d.rep.WouldCreate, note)
		return
	}
	created, err := d.e.cl[o].CreateAlbum(d.ctx, al.Name, al.Description, users)
	if err != nil {
		note.Reason = "create album: " + err.Error()
		d.note(&d.rep.Errors, note)
		d.e.log.Error("creating album failed (not retried; an album created anyway is adopted by name next cycle)",
			"op", "create_album", "side", o.String(), "kind", immich.KindOf(err).String(), "err", err)
		return
	}
	note.AlbumID = created.ID
	d.note(&d.rep.Created, note)
	d.e.log.Info("created album", "op", "create_album", "side", o.String(), "album_id", created.ID,
		"counterpart_of", al.ID, "shared_with", userIDs(users))
	d.albums[o] = append(d.albums[o], created)
	d.byID[o][created.ID] = created
	d.pin(s, al, created)
	if len(users) > 0 {
		d.st.Pairs[len(d.st.Pairs)-1].Created = &state.CreatedAlbum{Side: uint8(o), SharedWith: userIDs(users)}
	}
}

// pin records a new pair of al (side s) and x (the other side).
func (d *discovery) pin(s Side, al, x immich.Album) {
	ids := [2]string{}
	ids[s], ids[1-s] = al.ID, x.ID
	name := DiscoveredPairPrefix + strings.ToLower(ids[A])
	d.pinned[A][ids[A]], d.pinned[B][ids[B]] = true, true
	// A pairing (e.g. with an album a user re-created) clears the tombstones of both albums.
	kept := d.st.Tombstones[:0:0]
	for _, t := range d.st.Tombstones {
		if t.Side > 1 || t.AlbumID != ids[t.Side] {
			kept = append(kept, t)
		}
	}
	d.st.Tombstones = kept
	delete(d.tomb[A], ids[A])
	delete(d.tomb[B], ids[B])
	d.st.Pairs = append(d.st.Pairs, state.DiscoveredPair{Name: name, AlbumIDs: ids, Since: d.e.o.Now()})
	nameA := al.Name
	if s == B {
		nameA = x.Name
	}
	d.activate(name, ids, nameA)
	d.e.log.Info("paired albums", "op", "discovery", "pair", name, "album_a", ids[A], "album_b", ids[B])
}

func (d *discovery) activate(name string, ids [2]string, albumName string) {
	d.pairs = append(d.pairs, Pair{Name: name, A: AlbumRef{ID: ids[A]}, B: AlbumRef{ID: ids[B]}})
	d.rep.Pairs = append(d.rep.Pairs, DiscoveredPairInfo{Pair: name, AlbumIDs: ids, Name: albumName})
}

func (d *discovery) note(dst *[]AlbumNote, n AlbumNote) { *dst = append(*dst, n) }

// shareUsers resolves Options.DiscoveryShare for side s into album users with role
// Options.DiscoveryShareRole. Ids are used as given (the server rejects unknown ids, and
// then creates nothing); emails are looked up with GET /users, once per pass and only
// when an album is about to be created. The caller's own user is dropped (it owns the
// album) and duplicates are removed.
func (d *discovery) shareUsers(s Side) ([]immich.AlbumUser, error) {
	if d.shareDone[s] {
		return d.share[s], d.shareErr[s]
	}
	d.share[s], d.shareErr[s] = d.resolveShare(s)
	d.shareDone[s] = d.ctx.Err() == nil
	return d.share[s], d.shareErr[s]
}

func (d *discovery) resolveShare(s Side) ([]immich.AlbumUser, error) {
	want := d.e.o.DiscoveryShare[s]
	if len(want) == 0 {
		return nil, nil
	}
	var users []immich.User
	for _, w := range want {
		if isEmail(w) {
			l, err := d.e.cl[s].ListUsers(d.ctx)
			if err != nil {
				return nil, fmt.Errorf("list users on side %s: %w", s, err)
			}
			users = l
			break
		}
	}
	seen := map[string]bool{strings.ToLower(d.users[s]): true}
	var out []immich.AlbumUser
	for _, w := range want {
		id := w
		if isEmail(w) {
			id = ""
			for _, u := range users {
				if strings.EqualFold(u.Email, w) {
					id = u.ID
					break
				}
			}
			if id == "" {
				return nil, fmt.Errorf("no user with email %q on side %s (or the server hides other users)", w, s)
			}
		}
		k := strings.ToLower(id)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, immich.AlbumUser{UserID: id, Role: d.e.o.DiscoveryShareRole})
	}
	return out, nil
}

func isEmail(s string) bool { return strings.Contains(s, "@") }

func userIDs(us []immich.AlbumUser) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.UserID)
	}
	return out
}

// releasePair forgets the album claims of a pair that is no longer synced.
func (e *Engine) releasePair(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range e.resolved {
		if v == name {
			delete(e.resolved, k)
		}
	}
}

func loadStatusName(s state.LoadStatus) string {
	switch s {
	case state.Fresh:
		return "fresh"
	case state.Loaded:
		return "loaded"
	case state.CorruptMovedAside:
		return "corrupt_moved_aside"
	case state.FingerprintReset:
		return "fingerprint_reset"
	}
	return "unknown"
}
