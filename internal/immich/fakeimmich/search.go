package fakeimmich

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"immich-ferry/internal/immich"
)

func encodeCursor(off int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"offset":%d}`, off)))
}

func decodeCursor(c string) (int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(c)
		if err != nil {
			return 0, false
		}
	}
	var v struct {
		Offset *int `json:"offset"`
	}
	if json.Unmarshal(b, &v) != nil || v.Offset == nil || *v.Offset < 0 {
		return 0, false
	}
	return *v.Offset, true
}

type strFilter struct {
	Eq *string  `json:"eq"`
	In []string `json:"in"`
}

type searchReq struct {
	Filter *struct {
		AlbumIDs *struct {
			Any []string `json:"any"`
		} `json:"albumIds"`
		Checksum  *strFilter `json:"checksum"`
		TrashedAt *struct {
			Eq *string `json:"eq"`
		} `json:"trashedAt"`
		Visibility *strFilter `json:"visibility"`
	} `json:"filter"`
	OrderBy *struct {
		Field     string `json:"field"`
		Direction string `json:"direction"`
	} `json:"orderBy"`
	WithExif bool   `json:"withExif"`
	Size     *int   `json:"size"`
	Cursor   string `json:"cursor"`
}

func (s *Server) hSearch(w http.ResponseWriter, r *http.Request, u *user) {
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	flat := false
	for k := range raw {
		switch k {
		case "filter", "orderBy", "cursor", "size", "withExif", "withPeople", "withStacked":
		default:
			flat = true
		}
	}
	_, hasFilter := raw["filter"]
	_, hasOrder := raw["orderBy"]
	_, hasCursor := raw["cursor"]
	if flat && (hasFilter || hasOrder || hasCursor) {
		writeErr(w, 400, "Cannot mix legacy flat search fields with filter/orderBy/cursor")
		return
	}
	if flat || !hasFilter {
		writeErr(w, 400, "fakeimmich supports only structured search (filter/orderBy/cursor)")
		return
	}
	var req searchReq
	if err := json.Unmarshal(mustJSON(raw), &req); err != nil {
		badRequest(w, "body", "invalid search body: "+err.Error())
		return
	}
	size := 250
	if req.Size != nil {
		size = *req.Size
		if size < 1 || size > 1000 {
			badRequest(w, "size", "Number must be between 1 and 1000")
			return
		}
	}
	offset := 0
	if hasCursor {
		var ok bool
		if offset, ok = decodeCursor(req.Cursor); !ok {
			badRequest(w, "cursor", "Invalid cursor")
			return
		}
	}
	desc := true
	if req.OrderBy != nil {
		if req.OrderBy.Field != "" && req.OrderBy.Field != "fileCreatedAt" {
			badRequest(w, "orderBy.field", "Unsupported order field")
			return
		}
		desc = req.OrderBy.Direction != "asc"
	}
	for _, f := range []*strFilter{req.Filter.Checksum, req.Filter.Visibility} {
		if f != nil && f.In != nil && len(f.In) == 0 {
			badRequest(w, "filter", "in must have at least 1 item")
			return
		}
	}
	if a := req.Filter.AlbumIDs; a != nil && len(a.Any) == 0 {
		badRequest(w, "filter.albumIds.any", "must have at least 1 item")
		return
	}

	s.mu.Lock()
	hook := s.searchFn
	s.mu.Unlock()
	if hook != nil {
		hook(offset / size)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []*FakeAsset
	if req.Filter.AlbumIDs != nil {
		seen := map[string]bool{}
		for _, aid := range req.Filter.AlbumIDs.Any {
			al := s.albums[aid]
			if al == nil || s.albumRole(u.id, al) == "" {
				noAccess(w, "album.read")
				return
			}
			for _, id := range al.AssetIDs {
				if a := s.assets[id]; a != nil && !seen[id] {
					seen[id] = true
					candidates = append(candidates, a)
				}
			}
		}
	} else {
		for _, id := range s.order {
			if a := s.assets[id]; a.OwnerID == u.id {
				candidates = append(candidates, a)
			}
		}
	}
	var matched []*FakeAsset
	for _, a := range candidates {
		if a.Visibility == immich.VisibilityLocked {
			continue
		}
		f := req.Filter
		if f.TrashedAt != nil && f.TrashedAt.Eq == nil && a.IsTrashed {
			continue
		}
		if f.Checksum != nil {
			if f.Checksum.Eq != nil && a.Checksum != *f.Checksum.Eq {
				continue
			}
			if f.Checksum.In != nil && !contains(f.Checksum.In, a.Checksum) {
				continue
			}
		}
		if f.Visibility != nil {
			if f.Visibility.Eq != nil && string(a.Visibility) != *f.Visibility.Eq {
				continue
			}
			if f.Visibility.In != nil && !contains(f.Visibility.In, string(a.Visibility)) {
				continue
			}
		}
		matched = append(matched, a)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.FileCreatedAt.Equal(b.FileCreatedAt) {
			return a.FileCreatedAt.Before(b.FileCreatedAt) != desc
		}
		return (a.ID < b.ID) != desc
	})
	total := len(matched)
	end := min(offset+size, total)
	items := []any{}
	for i := offset; i < end; i++ {
		items = append(items, s.assetJSON(matched[i], u.id, req.WithExif))
	}
	var next any
	if end < total {
		next = encodeCursor(end)
	}
	writeJSON(w, 200, map[string]any{
		"albums": map[string]any{"total": 0, "count": 0, "items": []any{}, "facets": []any{}},
		"assets": map[string]any{"total": total, "count": len(items), "items": items, "facets": []any{},
			"nextCursor": next, "nextPage": nil},
	})
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
