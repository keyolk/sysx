package ui

import "testing"

func TestKeymapHasNoConflicts(t *testing.T) {
	for _, c := range Keymap().Conflicts(Stacks...) {
		t.Error(c)
	}
}
