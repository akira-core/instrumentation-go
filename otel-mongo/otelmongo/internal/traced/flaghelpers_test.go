package traced

// alwaysTrue is the func() bool stand-in for the propagation
// flag now that it is resolved per call rather than fixed at construction.
var alwaysTrue = func() bool { return true }
