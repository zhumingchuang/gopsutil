// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"errors"
	"strings"
	"testing"
)

func TestQueryDecoderRejectsAmbiguousOrInvalidObjects(t *testing.T) {
	for _, input := range []string{"null", "[]", `{}`, `{"pid":null}`, `{"pid":1,"expectedProcessKey":null}`, `{"pid":1,"pid":2}`, `{"pid":1,"extra":true}`, `{"pid":-1}`, `{"pid":1.5}`, `{"pid":1} {}`, `{"pid":1} trailing`, strings.Repeat(" ", 4097), string([]byte{0xff})} {
		if _, err := DecodeQuery[ProcessSelector]([]byte(input)); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("accepted %q: %v", input, err)
		}
	}
	q, err := DecodeQuery[ProcessSelector]([]byte(`{"pid":42,"expectedProcessKey":"key"}`))
	if err != nil || q.PID != 42 || q.ExpectedProcessKey != "key" {
		t.Fatalf("%+v %v", q, err)
	}
}
