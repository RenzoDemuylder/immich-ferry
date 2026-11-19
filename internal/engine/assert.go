package engine

import "immich-ferry/internal/immich"

// Compile-time check that the real client satisfies Immich.
var _ Immich = (*immich.Client)(nil)
