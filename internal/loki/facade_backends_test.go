package loki

import "time"

// resetBackendsState : pas de job ni de GPU en cache d'un test à l'autre.
func resetBackendsState() {
	lcMu.Lock()
	lcCur = nil
	lcLastSave = time.Time{}
	lcMu.Unlock()
	setBuildSink(nil)
	devCacheMu.Lock()
	devCache = map[string]devCacheEntry{}
	devCacheMu.Unlock()
}
