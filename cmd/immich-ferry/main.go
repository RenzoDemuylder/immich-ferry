// Command immich-ferry syncs an album between two Immich instances.
package main

import (
	"os"
)

// version is set via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}
