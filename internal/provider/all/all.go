// Package all links every channel into the binary.
//
// A channel registers itself with internal/provider from its own init function,
// so it exists only in a binary that imports its package. This is the one list
// of those imports: adding a channel means adding its line here and its
// definition in internal/channel.
package all

import (
	_ "orchids-api/internal/cline"
	_ "orchids-api/internal/grok"
	_ "orchids-api/internal/qoder"
	_ "orchids-api/internal/workbuddy"
)
