package main

import (
	"regexp"
	"testing"
)

func TestApplicationMetricsFitCollectorLabels(t *testing.T) {
	// Runtime collectors reject the whole snapshot if any label is invalid.
	label := regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	a := &App{appCore: &appCore{}}
	a.imagePermits = newOwnerImagePermits()
	a.regionalPermits = newRegionalImagePermits()
	for name := range a.snapshot().Metrics {
		if !label.MatchString(name) {
			t.Errorf("metric %q is outside the runtime collector contract", name)
		}
	}
}
