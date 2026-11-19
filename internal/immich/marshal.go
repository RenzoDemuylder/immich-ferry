package immich

import (
	"bytes"
	"encoding/json"
)

// objBuilder builds a JSON object with deterministic key order.
type objBuilder struct {
	buf bytes.Buffer
	n   int
}

func (b *objBuilder) add(key string, raw json.RawMessage) {
	if b.n == 0 {
		b.buf.WriteByte('{')
	} else {
		b.buf.WriteByte(',')
	}
	k, _ := json.Marshal(key)
	b.buf.Write(k)
	b.buf.WriteByte(':')
	b.buf.Write(raw)
	b.n++
}

func (b *objBuilder) bytes() []byte {
	if b.n == 0 {
		return []byte("{}")
	}
	b.buf.WriteByte('}')
	return b.buf.Bytes()
}

func addOpt[T any](b *objBuilder, key string, o Opt[T]) error {
	if !o.Set {
		return nil
	}
	raw, err := optJSON(o)
	if err != nil {
		return err
	}
	b.add(key, raw)
	return nil
}

// MarshalJSON emits only Set fields; Set with nil Val becomes null.
func (u AssetUpdate) MarshalJSON() ([]byte, error) {
	var b objBuilder
	for _, f := range []struct {
		key string
		err error
	}{
		{"description", addOpt(&b, "description", u.Description)},
		{"dateTimeOriginal", addOpt(&b, "dateTimeOriginal", u.DateTimeOriginal)},
		{"latitude", addOpt(&b, "latitude", u.Latitude)},
		{"longitude", addOpt(&b, "longitude", u.Longitude)},
		{"rating", addOpt(&b, "rating", u.Rating)},
		{"isFavorite", addOpt(&b, "isFavorite", u.IsFavorite)},
		{"visibility", addOpt(&b, "visibility", u.Visibility)},
		{"livePhotoVideoId", addOpt(&b, "livePhotoVideoId", u.LivePhotoVideoID)},
	} {
		if f.err != nil {
			return nil, f.err
		}
	}
	return b.bytes(), nil
}

// MarshalJSON emits only Set fields (albumName, description).
func (u AlbumInfoUpdate) MarshalJSON() ([]byte, error) {
	var b objBuilder
	if err := addOpt(&b, "albumName", u.Name); err != nil {
		return nil, err
	}
	if err := addOpt(&b, "description", u.Description); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}
