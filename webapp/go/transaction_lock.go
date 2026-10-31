package main

import "net/http"

func (a *App) lockWriteState() func() {
	a.mu.RLock()
	a.writeMu.Lock()
	a.view = a.published.Load().fork()
	return func() { a.view = nil; a.writeMu.Unlock(); a.mu.RUnlock() }
}
func (a *App) lockRequest(r *http.Request) func() {
	if r.URL.Path == "/api/initialize" {
		a.mu.Lock()
		return a.mu.Unlock
	}
	if r.Method == "GET" {
		return a.lockReadRequest(r)
	}
	return a.lockWriteState()
}
