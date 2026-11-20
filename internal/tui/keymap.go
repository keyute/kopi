package tui

import (
	"slices"

	"charm.land/bubbles/v2/key"
)

type keyMap struct {
	submit, newline, interrupt, cancel, quit, sendNow, toggleMode, background, stopAgent, expandAll,
	pageUp, pageDown, paste, historyPrev, historyNext, queueEdit, editPlan,
	complete, menuUp, menuDown, menuClose key.Binding
	// dismiss is the interrupt and cancel keys together: a prompt in front takes
	// either as its way out. It has no action name of its own.
	dismiss key.Binding
}

// bindings names each binding by its config action. A binding with no keys
// never matches, so an action missing from keys stays disabled.
func (km *keyMap) bindings() map[string]*key.Binding {
	return map[string]*key.Binding{
		"submit":       &km.submit,
		"newline":      &km.newline,
		"interrupt":    &km.interrupt,
		"cancel":       &km.cancel,
		"quit":         &km.quit,
		"send-now":     &km.sendNow,
		"toggle-mode":  &km.toggleMode,
		"background":   &km.background,
		"stop-agent":   &km.stopAgent,
		"expand-all":   &km.expandAll,
		"page-up":      &km.pageUp,
		"page-down":    &km.pageDown,
		"paste":        &km.paste,
		"history-prev": &km.historyPrev,
		"history-next": &km.historyNext,
		"queue-edit":   &km.queueEdit,
		"edit-plan":    &km.editPlan,
		"complete":     &km.complete,
		"menu-up":      &km.menuUp,
		"menu-down":    &km.menuDown,
		"menu-close":   &km.menuClose,
	}
}

func newKeyMap(keys map[string][]string) keyMap {
	var km keyMap
	for name, b := range km.bindings() {
		b.SetKeys(keys[name]...)
	}
	km.dismiss.SetKeys(slices.Concat(km.interrupt.Keys(), km.cancel.Keys())...)
	return km
}
