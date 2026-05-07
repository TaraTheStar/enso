package slowthing

import (
	"reflect"
	"testing"
)

func TestDedupe(t *testing.T) {
	got := Dedupe([]string{"a", "b", "a", "c", "b", "d"})
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := Dedupe(nil); len(got) != 0 {
		t.Errorf("nil input: got %v, want empty", got)
	}
}
