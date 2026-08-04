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

func TestGetStringSliceCustomDelimiter(t *testing.T) {
	got := GetStringSlice("a,b,,c", ",")
	want := []string{"a", "b", "c"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetStringSlice = %#v, want %#v", got, want)
	}

	if got := GetStringSlice("a;b", ","); !reflect.DeepEqual(got, []string{"a;b"}) {
		t.Errorf("GetStringSlice = %#v, want the input left whole", got)
	}
}
