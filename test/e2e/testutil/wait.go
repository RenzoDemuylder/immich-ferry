package testutil

import (
	"context"
	"testing"
	"time"
)

// Eventually polls cond every interval until it returns true or timeout elapses, then fails t with the last message.
func Eventually(t testing.TB, timeout, interval time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %s", timeout, last)
			return
		}
		time.Sleep(interval)
	}
}

// WaitExtracted waits until metadata extraction populated exifInfo.dateTimeOriginal.
func WaitExtracted(t testing.TB, api *API, assetID string) {
	t.Helper()
	Eventually(t, 120*time.Second, time.Second, func() (bool, string) {
		a, err := api.GetAssetTyped(context.Background(), assetID)
		if err != nil {
			return false, err.Error()
		}
		if a.Exif != nil && a.Exif.DateTimeOriginal != nil {
			return true, ""
		}
		return false, "asset " + assetID + " has no exifInfo.dateTimeOriginal yet"
	})
}

// WaitQueuesIdle waits for all queues to be idle on two consecutive polls 1s apart. api needs admin rights.
func WaitQueuesIdle(t testing.TB, api *API) {
	t.Helper()
	streak := 0
	Eventually(t, 120*time.Second, time.Second, func() (bool, string) {
		idle, busy, err := api.QueuesIdle(context.Background())
		if err != nil {
			streak = 0
			return false, err.Error()
		}
		if !idle {
			streak = 0
			return false, "busy: " + busy
		}
		streak++
		return streak >= 2, "idle streak " + time.Now().Format("15:04:05")
	})
}
