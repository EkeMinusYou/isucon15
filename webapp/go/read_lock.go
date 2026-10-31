package main

import "net/http"

// Requests read only fully published state. Preparation and journal waits do
// not hold this lock; publication changes all related records together.
func (a *App) lockReadView() func() {
	a.mu.RLock()
	a.viewMu.RLock()
	a.s = a.appCore.s
	return func() { a.forgetView(); a.viewMu.RUnlock(); a.mu.RUnlock() }
}
func (a *App) lockReadRequest(_ *http.Request) func() { return a.lockReadView() }
func (a *App) lockUserRead(_ string) func()           { return a.lockReadView() }
