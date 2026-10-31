package api

import "example.com/fixture/svc"

// API depends on svc, so transitively on core.
func API() int { return svc.Svc() + 1 }
