package tui

import (
	"maps"
	"slices"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/config"
)

func TestKeyMapNamesMatchConfig(t *testing.T) {
	var km keyMap
	assert.ElementsMatch(t, config.KeyActions, slices.Collect(maps.Keys(km.bindings())))
}

func TestNewKeyMapBindsConfiguredKeysOnly(t *testing.T) {
	km := newKeyMap(map[string][]string{"submit": {"ctrl+s", "enter"}})

	assert.True(t, key.Matches(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}, km.submit))
	assert.True(t, key.Matches(enter, km.submit))
	assert.False(t, key.Matches(ctrlC, km.cancel), "an action without keys never fires")
	assert.False(t, km.cancel.Enabled())
}
