package tui

import (
	"fmt"
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
	assert.Equal(t, []string{rule40, "  Paste the code", "", "❭ code", rule40}, plainLoginRows(ev, 0))
}

func TestLoginRowsMaskASecret(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := newLoginDialog(event.LoginRequested{Message: "API key", Placeholder: "key", Secret: true}, &km, mocha())
	assert.Equal(t, "❭ key", plainRows(loginRows(d, 40, 3, 100, mocha()))[3])

	d.paste("sk-secret")
	rows := plainRows(loginRows(d, 40, 3, 100, mocha()))
	assert.Equal(t, "❭ •••••••••", rows[3])
	assert.NotContains(t, strings.Join(rows, "\n"), "secret")
}

func TestLoginRowsIndentLaterRowsOfAMultiLineDraft(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := newLoginDialog(event.LoginRequested{Message: "Code"}, &km, mocha())
	d.paste("first\nsecond")
	rows := plainRows(loginRows(d, 40, 3, 100, mocha()))
	assert.Equal(t, []string{"❭ first", "  second"}, rows[3:5])
	require.Len(t, d.areas, 1)
	assert.Equal(t, 2, d.areas[0].x)
	assert.Equal(t, 3, d.areas[0].y)
}

func TestLoginRowsKeepTheSelectionBetweenRules(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	ev := event.LoginRequested{Message: strings.Repeat("Long title ", 20), Provider: strings.Repeat("provider ", 20)}
	for i := range 7 {
		ev.Options = append(ev.Options, event.LoginOption{Label: fmt.Sprintf("Method %d", i+1), Description: strings.Repeat("wrapped description ", 8)})
	}
	d := newLoginDialog(ev, &km, mocha())
	for _, sel := range []int{0, 3, 6} {
		d.move(sel)
		for _, height := range []int{3, 5, 9, 4, 100} {
			rows := plainRows(loginRows(d, 40, 3, height, mocha()))
			require.LessOrEqual(t, len(rows), height)
			assert.Equal(t, rule40, rows[0])
			assert.Equal(t, rule40, rows[len(rows)-1])
			assert.Contains(t, strings.Join(rows, "\n"), fmt.Sprintf("❭ %d. Method %d", sel+1, sel+1))
		}
	}
	for _, key := range []tea.KeyPressMsg{up, up, up, down, down} {
		d.key(key, &km)
		rows := plainRows(loginRows(d, 40, 3, 5, mocha()))
		assert.Contains(t, strings.Join(rows, "\n"), fmt.Sprintf("❭ %d. Method %d", d.sel+1, d.sel+1))
	}
}

func TestLoginRowsPinTheTitleAndWindowALongChoice(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	ev := event.LoginRequested{Message: "Choose a provider"}
	for i := range 40 {
		ev.Options = append(ev.Options, event.LoginOption{Label: fmt.Sprintf("Provider %d", i+1)})
	}
	d := newLoginDialog(ev, &km, mocha())
	for _, sel := range []int{0, 20, 39, 7} {
		d.move(sel)
		for _, height := range []int{4, 5, 6, 9, 20} {
			rows := plainRows(loginRows(d, 40, 3, height, mocha()))
			require.LessOrEqual(t, len(rows), height)
			assert.Equal(t, rule40, rows[0])
			assert.Equal(t, rule40, rows[len(rows)-1])
			assert.Contains(t, rows, fmt.Sprintf("❭ %d. Provider %d", sel+1, sel+1))
			assert.Contains(t, rows, fmt.Sprintf("  %d of 40", sel+1))
			if height >= 5 {
				assert.Equal(t, "  Choose a provider", rows[1])
			}
		}
	}
}

func TestLoginRowsTwoRowsKeepBothRulesWithoutFieldAreas(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := newLoginDialog(event.LoginRequested{Message: "Code"}, &km, mocha())
	loginRows(d, 40, 3, 100, mocha())
	require.NotEmpty(t, d.areas)

	rows := plainRows(loginRows(d, 40, 3, 2, mocha()))
	assert.Equal(t, []string{rule40, rule40}, rows)
	assert.Empty(t, d.areas)
}

func TestLoginResizeKeepsRulesAboveFooterAndFieldGeometry(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(fmt.Sprintf("secret=%t", secret), func(t *testing.T) {
			m, _ := openLogin(t, event.LoginRequested{Message: strings.Repeat("Long title ", 40), Provider: strings.Repeat("provider ", 20), Secret: secret})
			pasteText(m, "private\nanswer")
			for _, height := range []int{10, 6, 20, 8} {
				m.Update(tea.WindowSizeMsg{Width: 40, Height: height})
				rows := screen(m)
				require.Len(t, rows, height)
				assert.Equal(t, rule40, rows[height-2], "bottom rule immediately above footer")
				assert.Equal(t, rule40, plainRows(m.prompt)[0])
				assert.Contains(t, strings.Join(rows, "\n"), "❭ ", "field row visible")
				if secret {
					assert.NotContains(t, strings.Join(rows, "\n"), "private")
					assert.Contains(t, strings.Join(rows, "\n"), strings.Repeat(secretMark, len("privateanswer")))
					assert.Empty(t, m.geo.fields)
				} else {
					require.Len(t, m.geo.fields, 1)
					fa := m.geo.fields[0]
					assert.Equal(t, 2, fa.x)
					assert.Greater(t, fa.h, 0)
					assert.LessOrEqual(t, fa.y+fa.h, height-2)
					assert.Contains(t, []string{"❭ private", "❭ answer", "  answer"}, rows[fa.y])
				}
			}
		})
	}
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

func TestLoginSecretAnswerDropsLineBreaks(t *testing.T) {
	m, c := openLogin(t, event.LoginRequested{Token: 2, Message: "API key", Secret: true})
	pasteText(m, "sk-\r\nabc\ndef")
	press(m, enter)
	assert.Equal(t, []loginAnswer{{2, "sk-abcdef"}}, c.loginAnswers)
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

func TestLoginDialogSurvivesSessionChangeAndTurnEnd(t *testing.T) {
	m, _ := openLogin(t, event.LoginRequested{Token: 1, Message: "Code"})
	send(m, event.SessionChanged{})
	assert.NotNil(t, m.login)
	assert.NotNil(t, m.dialog)
	send(m, event.TurnEnd{})
	assert.NotNil(t, m.login)
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
