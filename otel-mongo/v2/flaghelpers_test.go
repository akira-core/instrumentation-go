package otelmongo

// alwaysTrue / alwaysFalse are the func() bool stand-ins for flags that are now
// resolved per call rather than fixed at construction.
var (
	alwaysTrue  = func() bool { return true }
	alwaysFalse = func() bool { return false }
)
