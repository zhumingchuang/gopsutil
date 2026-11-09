// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResultBufferRetryPinsExactBytes(t *testing.T) {
	var store ResultStore
	id, code := store.Put(map[string]string{"sample": "before"})
	if code != ABIOK {
		t.Fatal(code)
	}
	small := []byte{0x55}
	required, code := store.Read(id, small)
	if code != ABIBufferTooSmall || small[0] != 0x55 {
		t.Fatal("truncated/modified short buffer")
	}
	_, _ = store.Put(map[string]string{"sample": "after"})
	buffer := make([]byte, required)
	_, code = store.Read(id, buffer)
	if code != ABIOK || buffer[len(buffer)-1] != 0 {
		t.Fatal(code)
	}
	var value map[string]string
	if json.Unmarshal(buffer[:len(buffer)-1], &value) != nil || value["sample"] != "before" {
		t.Fatal(string(buffer))
	}
	if store.Release(id) != ABIOK {
		t.Fatal("release failed")
	}
	if _, code = store.Read(id, buffer); code != ABIResultExpired {
		t.Fatal(code)
	}
}
func TestResultStoreBoundedAndExpired(t *testing.T) {
	var store ResultStore
	for i := 0; i < 8; i++ {
		if _, code := store.Put(i); code != ABIOK {
			t.Fatal(code)
		}
	}
	if _, code := store.Put(9); code != ABILimitExceeded {
		t.Fatal(code)
	}
	store.entries[1] = frozenResult{body: []byte(`{}`), createdAt: time.Now().Add(-time.Minute)}
	if _, code := store.Read(1, nil); code != ABIResultExpired {
		t.Fatal(code)
	}
	if _, code := store.Put(10); code != ABIOK {
		t.Fatal(code)
	}
	store.Clear()
	if len(store.entries) != 0 {
		t.Fatal("clear failed")
	}
}
