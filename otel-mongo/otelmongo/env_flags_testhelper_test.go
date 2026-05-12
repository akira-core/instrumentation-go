package otelmongo

import "sync"

// resetPropEnabledCacheForTest re-arms the propagation-flag sync.Once so the next
// cachedPropagationEnabled() call re-reads the env. Test-only; not exported.
// Callers must invoke this AFTER t.Setenv changes envGlobalTracingEnabled or
// envMongoPropagationEnabled, otherwise the cached value from a prior test will leak.
func resetPropEnabledCacheForTest() {
	propEnabledOnce = sync.Once{}
	propEnabledFlag.Store(false)
}
