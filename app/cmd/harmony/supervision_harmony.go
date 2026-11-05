//go:build harmonyos

package main

import (
	"context"
	"errors"
	"time"
)

// Direct-child supervision only. No process discovery, saved-PID adoption or
// reboot recovery. The existing manager still owns validation, Start and Wait.
func (s *harmonyServer) disableSupervision() {
	s.mu.Lock()
	s.watchEpoch++
	cancel := s.watchCancel
	s.watchCancel = nil
	s.current.DesiredRunning = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Caller owns op; the monitor never calls the public start/restart methods.
func (s *harmonyServer) enableSupervisionLocked() {
	s.disableSupervision()
	ctx, cancel := context.WithCancel(s.lifetime)
	s.mu.Lock()
	s.current.DesiredRunning = true
	s.current.Restarts = 0
	s.watchCancel = cancel
	epoch := s.watchEpoch
	done := s.done
	s.mu.Unlock()
	go s.supervise(ctx, epoch, done)
}

func (s *harmonyServer) supervise(ctx context.Context, epoch uint64, done <-chan struct{}) {
	started := time.Now()
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= time.Minute {
			attempts = 0
		}
		for {
			if attempts >= s.restartLimit {
				s.op.Lock()
				s.mu.Lock()
				if s.watchEpoch == epoch && s.current.DesiredRunning {
					s.current.State = "failed"
					s.current.Error = "automatic restart limit reached; start manually after checking logs"
				}
				s.mu.Unlock()
				s.op.Unlock()
				return
			}
			delay := s.restartDelay * time.Duration(1<<min(attempts, 5))
			delay = min(delay, 30*time.Second)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			s.op.Lock()
			s.mu.Lock()
			valid := s.watchEpoch == epoch && s.current.DesiredRunning && ctx.Err() == nil
			config := s.current.Config
			s.mu.Unlock()
			if !valid {
				s.op.Unlock()
				return
			}
			attempts++
			s.mu.Lock()
			if s.watchEpoch == epoch && s.current.DesiredRunning && ctx.Err() == nil {
				s.current.Restarts = attempts
			}
			s.mu.Unlock()
			err := s.startLocked(ctx, config)
			s.mu.Lock()
			done = s.done
			s.mu.Unlock()
			s.op.Unlock()
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if err == nil {
				started = time.Now()
				break
			}
		}
	}
}
