package contract

// Handler is the producer of an HTTP contract another repository consumes.
func Handler() string { return "GET /widgets" }
