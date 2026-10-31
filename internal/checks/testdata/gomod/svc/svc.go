package svc

import "example.com/fixture/core"

// Svc depends on core.
func Svc() int { return core.Core() + 1 }
