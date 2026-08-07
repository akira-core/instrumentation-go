package otelmongo

// alwaysTrue is the func() bool stand-in for flags that are now
// resolved per call rather than fixed at construction.
var alwaysTrue = func() bool { return true }
