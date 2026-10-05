package messaging

import (
	"bytes"
	"switchyard/internal/outbox"
	"testing"
)

func TestEnvelopeStrictValidation(t *testing.T) {
	i := outbox.Item{ID: 12, Reference: outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "e", ObjectID: "event", Revision: 3}}
	b, err := Encode(i)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Decode(b)
	if err != nil || e.MessageID != i.MessageID() || e.Reference != i.Reference {
		t.Fatal("wire identity changed")
	}
	for _, bad := range [][]byte{
		append(append([]byte{}, b...), []byte(" {}")...),
		bytes.Replace(b, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(b, []byte(`"revision":3`), []byte(`"revision":0`), 1),
		bytes.Replace(b, []byte(`"kind":"event"`), []byte(`"kind":"other"`), 1),
		bytes.Replace(b, []byte(`"version":1`), []byte(`"secret":"bad","version":1`), 1),
		[]byte("null"), make([]byte, MaxEnvelopeBytes+1),
	} {
		if _, err := Decode(bad); err == nil {
			t.Fatalf("invalid envelope accepted: %.80s", bad)
		}
	}
}
