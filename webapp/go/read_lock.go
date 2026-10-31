package main

import "net/http"

func (a *App) lockReadRequest(_ *http.Request) func() {
	a.mu.RLock()
	a.view = a.published.Load()
	return func() { a.view = nil; a.mu.RUnlock() }
}
func (a *App) lockUserRead(_ string) func() { return a.lockReadRequest(nil) }
