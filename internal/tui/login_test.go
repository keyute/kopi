package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

func plainLoginRows(ev event.LoginRequested, sel int) []string {
	km := newKeyMap(config.DefaultKeys())
	d := newLoginDialog(ev, &km, mocha())
	d.sel = sel
	return plainRows(loginRows(d, 40, 3, 100, mocha()))
}

func TestLoginRowsListOptionsWithDescriptions(t *testing.T) {
	ev := event.LoginRequested{Provider: "openai", Message: "Choose a method", Options: []event.LoginOption{
		{ID: "oauth", Label: "Account", Description: "sign in with a browser"},
		{ID: "key", Label: "API key"},
	}}
	assert.Equal(t, []string{
		rule40,
		"  Choose a method",
		"  openai",
		"",
		"❭ 1. Account - sign in with a browser",
		"  2. API key",
		rule40,
	}, plainLoginRows(ev, 0))
}

func TestLoginRowsShowATextFieldWithItsPlaceholder(t *testing.T) {
	ev := event.LoginRequested{Message: "Paste the code", Placeholder: "code"}
	assert.Equal(t, []string{rule40, "  Paste the code", "", "❭ 1. Answer  code", rule40}, plainLoginRows(ev, 0))
}

func TestLoginRowsMaskASecret(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := newLoginDialog(event.LoginRequested{Message: "API key", Placeholder: "key", Secret: true}, &km, mocha())
	assert.Equal(t, "❭ 1. Answer  key", plainRows(loginRows(d, 40, 3, 100, mocha()))[3])

	d.paste("sk-secret")
	rows := plainRows(loginRows(d, 40, 3, 100, mocha()))
	assert.Equal(t, "❭ 1. Answer  •••••••••", rows[3])
	assert.NotContains(t, strings.Join(rows, "\n"), "secret")
}

func openLogin(t *testing.T, ev event.LoginRequested) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, 60, 20)
	send(m, ev)
	require.NotNil(t, m.login)
	return m, c
}

func TestLoginSecretNeverAppearsInTheView(t *testing.T) {
	m, _ := openLogin(t, event.LoginRequested{Token: 1, Message: "API key", Secret: true})
	for _, k := range "sk-typed" {
		press(m, tea.KeyPressMsg{Code: k, Text: string(k)})
	}
	pasteText(m, "-pasted")
	view := m.View().Content
	assert.Contains(t, view, strings.Repeat(secretMark, len("sk-typed-pasted")))
	assert.NotContains(t, view, "typed")
	assert.NotContains(t, view, "pasted")
	assert.Empty(t, m.geo.fields, "a secret has no selectable area")
}

func TestLoginPastesTheClipboardIntoTheMaskedField(t *testing.T) {
	m, _ := openLogin(t, event.LoginRequested{Token: 1, Message: "API key", Secret: true})
	m.Update(clipboardTextMsg{text: "sk-clip", field: m.dialog.field()})
	assert.Equal(t, "sk-clip", m.dialog.text())
	assert.NotContains(t, m.View().Content, "clip")
}

func TestLoginEnterAnswersWithTheTypedText(t *testing.T) {
	m, c := openLogin(t, event.LoginRequested{Token: 7, Message: "Code"})
	pasteText(m, "abc")
	press(m, enter)
	assert.Equal(t, []loginAnswer{{7, "abc"}}, c.loginAnswers)
	assert.Empty(t, m.dialog.text(), "the field is cleared once answered")
	assert.True(t, m.composer.empty(), "the answer never enters the composer")
}

func TestLoginEnterAnswersWithTheOptionID(t *testing.T) {
	m, c := openLogin(t, event.LoginRequested{Token: 3, Message: "Method", Options: []event.LoginOption{
		{ID: "oauth", Label: "Account"}, {ID: "key", Label: "API key"},
	}})
	press(m, tea.KeyPressMsg{Code: tea.KeyDown})
	press(m, enter)
	assert.Equal(t, []loginAnswer{{3, "key"}}, c.loginAnswers)
}

func TestLoginEscapeInterrupts(t *testing.T) {
	m, c := openLogin(t, event.LoginRequested{Token: 1, Message: "Code"})
	press(m, escape)
	assert.Equal(t, 1, c.interrupts)
	assert.Empty(t, c.loginAnswers)
	assert.NotNil(t, m.login, "the prompt stays until LoginEnded")
}

func TestLoginEndedClosesTheDialog(t *testing.T) {
	m, _ := openLogin(t, event.LoginRequested{Token: 4, Message: "Code"})
	send(m, event.LoginEnded{Token: 5})
	assert.NotNil(t, m.login, "another token's end is ignored")

	send(m, event.LoginEnded{Token: 4})
	assert.Nil(t, m.login)
	assert.Nil(t, m.dialog)
	assert.NotContains(t, strings.Join(screen(m), "\n"), "Code")
}

func TestLoginEndedKeepsAnApprovalDialog(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.ApprovalRequested{Token: 1, Title: "Edit a.go"})
	send(m, event.LoginEnded{Token: 1})
	assert.NotNil(t, m.dialog)
}

func TestSubmitFlashesWhileLoggingIn(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = app.ErrLoggingIn
	typeText(m, "hello")
	press(m, enter)
	assert.Equal(t, "a login is under way", m.flashText)
	assert.Equal(t, "hello", m.composer.value(), "the draft is kept")
}
