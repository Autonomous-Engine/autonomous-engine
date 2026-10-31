package nested

import "example.com/fixture/core"

// N is in a nested module and outside the fixture module graph.
func N() int { return core.Core() }
