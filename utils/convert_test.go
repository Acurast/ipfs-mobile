package utils

import (
	"reflect"
	"testing"
)

func TestGetStringSlice(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		// strings.Split("", ";") returns [""], which reached connectToPeers as a
		// single empty address and aborted the whole bootstrap list.
		{"empty", "", []string{}},
		{"whitespace only", "   ", []string{}},
		{"single", "a", []string{"a"}},
		{"multiple", "a;b;c", []string{"a", "b", "c"}},
		{"trailing delimiter", "a;b;", []string{"a", "b"}},
		{"blank entries", "a;;b", []string{"a", "b"}},
		{"padded entries", " a ; b ", []string{"a", "b"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GetStringSlice(test.input)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("GetStringSlice(%q) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}
