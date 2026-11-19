package engine

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"

	"immich-ferry/internal/immich"
)

const (
	downloadAttempts = 3
	tmpPattern       = "immich-ferry-*.tmp"
)

// errTmpBudget marks an asset that cannot fit the temp-space budget.
var errTmpBudget = errors.New("asset exceeds the temp budget (SYNC_TMP_BUDGET / tmp.sizeLimit)")

var errChecksumMismatch = errors.New("downloaded content does not match source checksum")

type tmpFile struct {
	f   *os.File
	sum string // base64 SHA1 of the content
}

func (t *tmpFile) cleanup() {
	if t == nil || t.f == nil {
		return
	}
	name := t.f.Name()
	_ = t.f.Close()
	_ = os.Remove(name)
	t.f = nil
}

func assetSize(a immich.Asset) int64 {
	if a.Exif != nil && a.Exif.FileSizeInByte != nil && *a.Exif.FileSizeInByte > 0 {
		return *a.Exif.FileSizeInByte
	}
	return 0
}

// checkTmpBudget fails fast, before any download, when the known size of a
// exceeds the whole budget.
func (r *run) checkTmpBudget(a immich.Asset) error {
	if t := r.o.TmpBudgetBytes; t > 0 {
		if n := assetSize(a); n > t {
			return fmt.Errorf("%w: asset %s is %d bytes, budget %d", errTmpBudget, a.ID, n, t)
		}
	}
	return nil
}

// budgetWriter aborts a download that writes more than the budget.
type budgetWriter struct {
	w        io.Writer
	limit, n int64
	exceeded bool
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	b.n += int64(len(p))
	if b.limit > 0 && b.n > b.limit {
		b.exceeded = true
		return 0, errTmpBudget
	}
	return b.w.Write(p)
}

// fetch downloads the original of a (on side s) into a temp file while
// hashing it. When verify is set the hash must equal want; mismatches and
// transient errors are retried by recreating the file (up to 3 attempts).
func (r *run) fetch(s Side, a immich.Asset, want string, verify bool) (*tmpFile, error) {
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(r.o.TmpDir, tmpPattern)
		if err != nil {
			return nil, fmt.Errorf("create temp file: %w", err)
		}
		tf := &tmpFile{f: f}
		h := sha1.New()
		bw := &budgetWriter{w: io.MultiWriter(f, h), limit: r.o.TmpBudgetBytes}
		_, err = r.e.cl[s].DownloadOriginal(r.ctx, a.ID, bw, func(n int64) {
			r.bytesDown.Add(n)
			r.byteTick()
		})
		if err != nil {
			tf.cleanup()
			if bw.exceeded {
				return nil, fmt.Errorf("%w: asset %s exceeded %d bytes while downloading", errTmpBudget, a.ID, r.o.TmpBudgetBytes)
			}
			if r.canceled(err) {
				return nil, err
			}
			lastErr = err
			if immich.KindOf(err) == immich.KindTransient && attempt < downloadAttempts {
				r.log.Warn("download failed, retrying", "phase", "copy", "side", s.String(), "asset_id", a.ID, "op", "download", "attempt", attempt, "err", err.Error())
				continue
			}
			return nil, err
		}
		tf.sum = sha1b64(h.Sum(nil))
		if verify && tf.sum != want {
			tf.cleanup()
			lastErr = fmt.Errorf("%w: asset %s got %s want %s", errChecksumMismatch, a.ID, tf.sum, want)
			r.log.Warn("checksum mismatch after download", "phase", "copy", "side", s.String(), "asset_id", a.ID, "op", "download", "attempt", attempt)
			continue
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			tf.cleanup()
			return nil, err
		}
		return tf, nil
	}
	return nil, lastErr
}

type uploadOpts struct {
	visibility immich.Visibility
	liveID     string
}

// upload sends tf to side s, using a's file metadata.
func (r *run) upload(s Side, a immich.Asset, tf *tmpFile, o uploadOpts) (immich.UploadResult, error) {
	mod := a.FileModifiedAt
	if mod.IsZero() {
		mod = a.FileCreatedAt
	}
	req := immich.UploadRequest{
		Filename: a.OriginalFileName, FileCreatedAt: a.FileCreatedAt, FileModifiedAt: mod,
		DurationMS: a.DurationMS, Visibility: o.visibility,
		LivePhotoVideoID: o.liveID, File: tf.f, Checksum: tf.sum,
	}
	res, err := r.e.cl[s].Upload(r.ctx, req, func(n int64) {
		r.bytesUp.Add(n)
		r.byteTick()
	})
	r.mut()
	return res, err
}
