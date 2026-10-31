package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type stateCommand struct {
	run  func()
	done chan any
}

// One executor owns mutable application state. HTTP goroutines receive and
// transmit bodies independently, then submit only the state-dependent work.
type stateExecutor struct {
	start       sync.Once
	commands    chan stateCommand
	requests    atomic.Uint64
	waitNS      atomic.Uint64
	executionNS atomic.Uint64
}

func runStateCommand(run func()) (failure any) {
	defer func() { failure = recover() }()
	run()
	return nil
}

func (e *stateExecutor) do(run func()) {
	e.start.Do(func() {
		e.commands = make(chan stateCommand)
		go func(commands <-chan stateCommand) {
			for command := range commands {
				failure := runStateCommand(command.run)
				command.run = nil
				command.done <- failure
			}
		}(e.commands)
	})
	started := time.Now()
	done := make(chan any, 1)
	e.commands <- stateCommand{run: func() {
		e.requests.Add(1)
		e.waitNS.Add(uint64(time.Since(started)))
		started := time.Now()
		defer func() { e.executionNS.Add(uint64(time.Since(started))) }()
		run()
	}, done: done}
	if failure := <-done; failure != nil {
		panic(failure)
	}
}

// Shutdown follows completion of HTTP handlers; no new commands can arrive.
func (e *stateExecutor) close() {
	if e.commands != nil {
		close(e.commands)
	}
}

func (a *App) withState(run func()) { a.executor.do(run) }
