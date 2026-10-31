package main

import "strconv"

const signatureCacheCapacity = 65536

type signatureFlight struct {
	done  chan struct{}
	value string
}

// Include every cryptographic input. In-flight work remains valid even if
// initialization replaces the application state before a response completes.
func signatureKey(id, secret string, exp int64) string {
	return id + ":" + strconv.FormatInt(exp, 10) + ":" + secret
}
func (a *App) reserveSignature(key string) (string, *signatureFlight, bool) {
	a.signatureMu.Lock()
	defer a.signatureMu.Unlock()
	if value := a.signatures[key]; value != "" {
		a.signatureHits.Add(1)
		return value, nil, false
	}
	if f := a.signatureFlights[key]; f != nil {
		a.signatureCoalesced.Add(1)
		return "", f, false
	}
	if a.signatureFlights == nil {
		a.signatureFlights = map[string]*signatureFlight{}
	}
	f := &signatureFlight{done: make(chan struct{})}
	a.signatureFlights[key] = f
	return "", f, true
}
func (a *App) finishSignature(key string, f *signatureFlight, value string) {
	a.signatureMu.Lock()
	defer a.signatureMu.Unlock()
	if a.signatures == nil {
		a.signatures = map[string]string{}
	}
	if _, exists := a.signatures[key]; !exists {
		if len(a.signatureOrder) < signatureCacheCapacity {
			a.signatureOrder = append(a.signatureOrder, key)
		} else {
			delete(a.signatures, a.signatureOrder[a.signatureNext])
			a.signatureOrder[a.signatureNext] = key
			a.signatureNext = (a.signatureNext + 1) % signatureCacheCapacity
			a.signatureEvictions.Add(1)
		}
	}
	a.signatures[key] = value
	f.value = value
	delete(a.signatureFlights, key)
	close(f.done)
}
