package main

// Queries see only fully published transactions. Remote persistence is outside
// this lock, so an unrelated user's master cannot stall snapshot acquisition.
func (a *App) lockReadView() func() {
	a.mu.RLock()
	a.viewMu.RLock()
	a.s = a.appCore.s
	return func() { a.viewMu.RUnlock(); a.mu.RUnlock() }
}
