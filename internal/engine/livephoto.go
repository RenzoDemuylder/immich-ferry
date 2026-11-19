package engine

import (
	"errors"
	"fmt"
	"sync"

	"immich-ferry/internal/immich"
)

var (
	errVisibleMotion = errors.New("target motion asset exists but is not hidden")
	errMotionGone    = errors.New("source motion asset is unavailable")
)

func isMotionSkip(err error) bool {
	return errors.Is(err, errVisibleMotion) || errors.Is(err, errMotionGone)
}

func (r *run) motionLock(s Side, checksum string) *sync.Mutex {
	k := s.String() + ":" + checksum
	r.motionMu.Lock()
	defer r.motionMu.Unlock()
	m := r.motionLks[k]
	if m == nil {
		m = &sync.Mutex{}
		r.motionLks[k] = m
	}
	return m
}

// ensureMotion makes sure the motion video motionID (on side from) exists
// hidden on side to and returns its id there. It never returns a target
// motion that is timeline/archive visible (errVisibleMotion).
func (r *run) ensureMotion(from, to Side, motionID string) (string, error) {
	cl := r.e.cl
	ma, err := cl[from].GetAsset(r.ctx, motionID)
	if err != nil {
		if immich.KindOf(err) == immich.KindNotFound && r.ctx.Err() == nil {
			return "", fmt.Errorf("%w: %v", errMotionGone, err)
		}
		return "", err
	}
	lk := r.motionLock(to, ma.Checksum)
	lk.Lock()
	defer lk.Unlock()

	chk, err := cl[to].UploadCheck(r.ctx, []immich.UploadCheckItem{{ID: ma.ID, Checksum: ma.Checksum}})
	if err != nil {
		return "", err
	}
	if len(chk) == 0 {
		return "", fmt.Errorf("upload check for motion %s returned no result", ma.ID)
	}
	if !chk[0].Accept {
		return r.useExistingMotion(to, chk[0].AssetID)
	}

	if err := r.checkTmpBudget(ma); err != nil {
		return "", err
	}
	release, err := r.budget.acquire(r.ctx, assetSize(ma))
	if err != nil {
		return "", err
	}
	defer release()
	tf, err := r.fetch(from, ma, ma.Checksum, ma.LibraryID == nil)
	if err != nil {
		return "", err
	}
	defer tf.cleanup()
	res, err := r.upload(to, ma, tf, uploadOpts{visibility: immich.VisibilityHidden})
	if err != nil {
		return "", err
	}
	if res.Duplicate {
		return r.useExistingMotion(to, res.ID)
	}
	return res.ID, nil
}

func (r *run) useExistingMotion(to Side, id string) (string, error) {
	cl := r.e.cl[to]
	g, err := cl.GetAsset(r.ctx, id)
	if err != nil {
		if immich.KindOf(err) == immich.KindNotFound && r.ctx.Err() == nil {
			return "", fmt.Errorf("%w: existing motion %s unreadable: %v", errVisibleMotion, id, err)
		}
		return "", err
	}
	if g.IsTrashed {
		if !r.o.RestoreTrashed || r.bootstrap {
			return "", fmt.Errorf("%w: existing motion %s is trashed", errVisibleMotion, id)
		}
		err := cl.RestoreFromTrash(r.ctx, []string{id})
		r.mut()
		if err != nil {
			return "", err
		}
		g, err = cl.GetAsset(r.ctx, id)
		if err != nil {
			return "", err
		}
		if g.IsTrashed {
			return "", fmt.Errorf("%w: existing motion %s still trashed after restore", errVisibleMotion, id)
		}
		r.count(func(s *RunSummary) { dirInc(&s.Restored, 1-to) })
	}
	if g.Visibility != immich.VisibilityHidden {
		return "", fmt.Errorf("%w: existing motion %s has visibility %s", errVisibleMotion, id, g.Visibility)
	}
	return g.ID, nil
}

// linkLive copies the motion to side to and links targetStillID to it.
func (r *run) linkLive(from, to Side, motionSrcID, targetStillID string) error {
	mid, err := r.ensureMotion(from, to, motionSrcID)
	if err != nil {
		return err
	}
	_, err = r.e.cl[to].UpdateAsset(r.ctx, targetStillID, immich.AssetUpdate{LivePhotoVideoID: immich.Some(mid)})
	r.mut()
	return err
}
