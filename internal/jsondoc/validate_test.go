package jsondoc

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, data := range []string{
		`{}`, `{"number":123456789012345678901234567890e9999,"opaque":[null,true,"value"]}`,
		`{"a":{"same":1},"b":{"same":2}}`,
	} {
		if err := Validate([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range []string{
		``, `{"a":`, `{} {}`, `{"a":1,"a":2}`, `{"a":1,"\u0061":2}`,
		`{"nested":[{"a":1,"a":2}]}`, "{\"a\":\"\xff\"}", `[1,]`,
		strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130),
	} {
		if Validate([]byte(data)) == nil {
			t.Error("accepted invalid JSON")
		}
	}
}
