package providerlogbridge

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestForgejoLogIndexesUseBoundedSignedVarints(t *testing.T) {
	encode := func(values ...int64) []byte {
		var result []byte
		for _, value := range values {
			result = binary.AppendVarint(result, value)
		}
		return result
	}
	got, err := decodeLogIndexes(encode(0, 128), 2, 132)
	if err != nil || !reflect.DeepEqual(got, []int64{0, 128, 132}) {
		t.Fatalf("signed absolute offsets: %v %v", got, err)
	}
	for _, test := range []struct {
		name        string
		data        []byte
		count, size int64
	}{
		{"negative", encode(0, -1), 2, 132},
		{"duplicate", encode(0, 0), 2, 132},
		{"descending", encode(0, 128, 100), 3, 132},
		{"nonzero_first", encode(1, 128), 2, 132},
		{"outside_text", encode(0, 132), 2, 132},
		{"missing_index", encode(0), 2, 132},
		{"extra_index", encode(0, 100, 128), 2, 132},
		{"truncated_varint", []byte{0, 128}, 2, 132},
		{"overflow_varint", []byte{0, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255}, 2, 132},
		{"count_budget", nil, 1<<20 + 1, 132},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := decodeLogIndexes(test.data, test.count, test.size); err == nil {
				t.Fatalf("invalid index accepted: %v", got)
			}
		})
	}
}
