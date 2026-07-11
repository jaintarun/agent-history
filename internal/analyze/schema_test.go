package analyze

import (
	"encoding/json"
	"testing"
)

func FuzzStructuredOutputDecoders(f *testing.F) {
	f.Add(byte(0), []byte(`{"title":"Leaf","goal":"Goal","summary":"Summary","detail":"Detail","outcome":"done","entities":[],"files":[],"errors":[],"evidence":[1]}`))
	f.Add(byte(1), []byte(`{"same_topic":true,"confidence":0.5,"new_title":""}`))
	f.Add(byte(2), []byte(`{"title":"Topic","summary":"Summary","detail":"Detail","evidence":[1]}`))
	f.Add(byte(3), []byte(`{"title":"Session","summary":"Summary"}`))
	f.Add(byte(0), []byte(`not json`))
	f.Fuzz(func(t *testing.T, kind byte, input []byte) {
		raw := json.RawMessage(input)
		switch kind % 4 {
		case 0:
			_, _ = decodeLeaf(raw, 0, 10)
		case 1:
			_, _ = decodeBoundary(raw)
		case 2:
			_, _ = decodeRollup(raw, 0, 10)
		case 3:
			_, _ = decodeSession(raw)
		}
	})
}
