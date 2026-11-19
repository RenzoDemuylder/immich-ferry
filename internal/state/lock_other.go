//go:build !unix

package state

import (
	"errors"
	"os"
)

func flockExclusive(*os.File) error {
	return errors.New("state: file locking is not supported on this platform")
}
