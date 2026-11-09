// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	ABIOK = iota
	ABIInvalidArgument
	ABINotRunning
	ABINotReady
	ABISnapshotExpired
	ABIProcessNotFound
	ABIIdentityChanged
	ABILimitExceeded
	ABIInternalError
	ABIBufferTooSmall
	ABIResultExpired
	ABIAlreadyRunning
)

func ABIError(err error) int {
	switch {
	case err == nil:
		return ABIOK
	case errors.Is(err, ErrInvalidQuery):
		return ABIInvalidArgument
	case errors.Is(err, ErrClosed):
		return ABINotRunning
	case errors.Is(err, ErrNotCollecting):
		return ABINotReady
	case errors.Is(err, ErrSnapshotExpired):
		return ABISnapshotExpired
	case errors.Is(err, ErrProcessNotFound):
		return ABIProcessNotFound
	case errors.Is(err, ErrIdentityChanged):
		return ABIIdentityChanged
	case errors.Is(err, ErrResourceLimited):
		return ABILimitExceeded
	default:
		return ABIInternalError
	}
}

type frozenResult struct {
	body      []byte
	createdAt time.Time
}

// ResultStore pins serialized bytes independently of the sampling generation.
// A buffer-size retry therefore reads exactly the result the caller requested.
type ResultStore struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]frozenResult
}

func (s *ResultStore) Put(value any) (uint64, int) {
	body, err := json.Marshal(value)
	if err != nil {
		return 0, ABIInternalError
	}
	if len(body) > MaximumSnapshotBytes {
		return 0, ABILimitExceeded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[uint64]frozenResult)
	}
	for id, result := range s.entries {
		if time.Since(result.createdAt) > SnapshotTTL {
			delete(s.entries, id)
		}
	}
	if len(s.entries) >= 8 {
		return 0, ABILimitExceeded
	}
	s.next++
	if s.next == 0 {
		s.next++
	}
	s.entries[s.next] = frozenResult{body: body, createdAt: time.Now()}
	return s.next, ABIOK
}

func (s *ResultStore) Read(id uint64, buffer []byte) (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.entries[id]
	if !ok {
		return 0, ABIResultExpired
	}
	if time.Since(result.createdAt) > SnapshotTTL {
		delete(s.entries, id)
		return 0, ABIResultExpired
	}
	required := len(result.body) + 1
	if len(buffer) < required {
		return required, ABIBufferTooSmall
	}
	copy(buffer, result.body)
	buffer[len(result.body)] = 0
	return required, ABIOK
}
func (s *ResultStore) Release(id uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return ABIResultExpired
	}
	delete(s.entries, id)
	return ABIOK
}
func (s *ResultStore) Clear() { s.mu.Lock(); defer s.mu.Unlock(); s.entries = nil }
