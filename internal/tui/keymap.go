package tui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	submit, newline, interrupt, cancel, quit, sendNow, toggleMode, background, expandAll,
	pageUp, pageDown, paste, historyPrev, historyNext, queueEdit, editPlan,
	complete, menuUp, menuDown, menuClose key.Binding
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
	return km
}
