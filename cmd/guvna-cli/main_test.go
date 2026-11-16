package main

import (
	"reflect"
	"testing"
)

func TestReorderArgsTrailingFlagDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reorderArgs panicked: %v", r)
		}
	}()
	if got := reorderArgs([]string{"--token"}); !reflect.DeepEqual(got, []string{"--token"}) {
		t.Fatalf("got %v", got)
	}
	if got := reorderArgs([]string{"mychain", "--token"}); !reflect.DeepEqual(got, []string{"--token", "mychain"}) {
		t.Fatalf("got %v", got)
	}
}

func TestReorderArgsMovesFlagsFirst(t *testing.T) {
	got := reorderArgs([]string{"mychain", "--token", "sekret"})
	want := []string{"--token", "sekret", "mychain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got = reorderArgs([]string{"--json", "mychain"})
	want = []string{"--json", "mychain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
