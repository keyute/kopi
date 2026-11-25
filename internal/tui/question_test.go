package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func choice(header string, multi bool, labels ...string) tool.Question {
	q := tool.Question{Question: "Which " + header + "?", Header: header, MultiSelect: multi}
	for _, l := range labels {
		q.Options = append(q.Options, tool.QuestionOption{Label: l, Description: "about " + l})
	}
	return q
}

func plainQuestionRows(qs []tool.Question, tab, width, maxPreview, maxRows int) []string {
	km := newKeyMap(config.DefaultKeys())
	q := newQuestionDialog(event.QuestionRequested{Token: 1, Questions: qs}, &km, mocha())
	q.tab = tab
	return plainRows(q.dialog(width, mocha()).rows(width, maxRows, maxPreview, mocha()))
}

func TestQuestionRowsShowTitleOptionsDescriptionsAndTheCustomRow(t *testing.T) {
	rule60 := strings.Repeat("─", 60)
	assert.Equal(t, []string{
		rule60,
		"  Which db?",
		"",
		"❭ 1. Postgres",
		"  about Postgres",
		"  2. SQLite",
		"  about SQLite",
		"  3. Type something.",
		"",
		"  enter to choose · tab to add a note · esc to dismiss",
		rule60,
	}, plainQuestionRows([]tool.Question{choice("db", false, "Postgres", "SQLite")}, 0, 60, 6, 100))
}

func TestQuestionRowsMarkMultiSelectOptions(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	qs := []tool.Question{choice("db", true, "A", "B")}
	q := newQuestionDialog(event.QuestionRequested{Questions: qs}, &km, mocha())
	q.pages[0].picked[1] = true
	rows := plainRows(q.dialog(80, mocha()).rows(80, 100, 6, mocha()))
	assert.Equal(t, "  space to toggle · enter to continue · tab to add a note · esc to dismiss", rows[len(rows)-2])
	assert.Equal(t, []string{"❭ 1. [ ] A", "  about A", "  2. [✔] B", "  about B", "  3. [ ] Type something."}, rows[3:8])
}

func TestQuestionRowsDrawTheTabStripOnlyForSeveralQuestions(t *testing.T) {
	rows := plainQuestionRows([]tool.Question{choice("db", false, "A", "B")}, 0, 60, 6, 100)
	assert.Equal(t, "  Which db?", rows[1], "the title sits under the rule")

	qs := []tool.Question{choice("Alpha", false, "A", "B"), choice("Bravo", false, "A", "B"), choice("Charlie", false, "A", "B"), choice("Delta", false, "A", "B")}
	several := plainQuestionRows(qs, 1, 60, 6, 100)
	assert.Equal(t, "   ☐ Alpha  ☐ Bravo  ☐ Charlie  ☐ Delta  ✔ Submit", several[1])
	assert.Equal(t, []string{"", "  Which Bravo?"}, several[2:4], "a blank row, then the title")
	assert.Equal(t, "   ☐ Delta  ✔ Submit", plainQuestionRows(qs, 3, 30, 6, 100)[1], "a narrow strip starts at the active tab")
	narrow := plainQuestionRows(qs, 2, 30, 6, 100)[1]
	assert.Contains(t, narrow, "☐ Charlie")
	assert.True(t, strings.HasSuffix(narrow, "…"), narrow)
	assert.LessOrEqual(t, ansi.StringWidth(narrow), 30)

	long := []tool.Question{choice("A header of more than sixteen", false, "A", "B"), qs[0]}
	assert.Equal(t, "   ☐ A header of mor…  ☐ Alpha  ✔ Submit", plainQuestionRows(long, 0, 60, 6, 100)[1])
}

func TestQuestionRowsMarkAnsweredTabs(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	qs := []tool.Question{choice("One", false, "A", "B"), choice("Two", false, "A", "B")}
	q := newQuestionDialog(event.QuestionRequested{Questions: qs}, &km, mocha())
	q.pages[1].chosen = 0
	assert.Equal(t, "   ☐ One  ☑ Two  ✔ Submit", plainRows(q.dialog(60, mocha()).rows(60, 100, 6, mocha()))[1])
}

func TestQuestionRowsReviewShowAnswersAndGaps(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	qs := []tool.Question{choice("One", false, "A", "B"), choice("Two", true, "C", "D"), choice("Three", false, "E", "F")}
	q := newQuestionDialog(event.QuestionRequested{Questions: qs}, &km, mocha())
	q.pages[0].chosen = 1
	q.pages[0].notes[1] = "soon"
	q.pages[1].picked = []bool{true, true}
	q.pages[1].custom().paste("mine")
	q.tab = 3
	rows := plainRows(q.dialog(70, mocha()).rows(70, 100, 20, mocha()))
	assert.Equal(t, []string{
		"",
		"  1. Which One?",
		"  ↳ B — soon",
		"  2. Which Two?",
		"  ↳ C; D; mine",
		"  3. Which Three?",
		"  ↳ Answer required",
		"",
		"  enter to submit · ← to go back · esc to dismiss",
	}, rows[2:11])
	assert.Len(t, rows, 12)
}

func TestQuestionRowsKeepTheSelectionAndRulesWhenShort(t *testing.T) {
	rule40 := strings.Repeat("─", 40)
	km := newKeyMap(config.DefaultKeys())
	qs := []tool.Question{choice("One", false, "A", "B", "C", "D"), choice("Two", false, "A", "B")}
	q := newQuestionDialog(event.QuestionRequested{Questions: qs}, &km, mocha())
	for _, sel := range []int{0, 2, 3, 4} {
		q.pages[0].d.move(sel)
		for _, maxRows := range []int{4, 5, 6, 9} {
			rows := plainRows(q.dialog(40, mocha()).rows(40, maxRows, 6, mocha()))
			require.LessOrEqual(t, len(rows), maxRows)
			assert.Equal(t, rule40, rows[0])
			assert.Equal(t, rule40, rows[len(rows)-1])
			assert.Contains(t, rows[1], "☐ One", "the strip stays")
			assert.Contains(t, strings.Join(rows, "\n"), fmt.Sprintf("❭ %d. ", sel+1))
		}
	}
	assert.Equal(t, []string{rule40, "❭ 5. Type something.", rule40}, plainRows(q.dialog(40, mocha()).rows(40, 3, 6, mocha())), "the selected option fits between the rules")
	assert.Equal(t, []fieldArea{{c: q.pages[0].custom(), x: 5, y: 1, h: 1}}, q.current().areas)
	assert.Equal(t, []string{rule40, rule40}, plainRows(q.dialog(40, mocha()).rows(40, 2, 6, mocha())))

	one := newQuestionDialog(event.QuestionRequested{Questions: qs[:1]}, &km, mocha())
	assert.Len(t, one.dialog(40, mocha()).rows(40, 3, 6, mocha()), 3)
}

func TestQuestionRowsTrimFieldAreasToTheWindow(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	q := newQuestionDialog(event.QuestionRequested{Questions: []tool.Question{choice("One", false, "A", "B", "C")}}, &km, mocha())
	d := q.pages[0].d
	d.move(3)
	rows := plainRows(q.dialog(40, mocha()).rows(40, 4, 6, mocha()))
	require.Len(t, d.areas, 1)
	assert.Contains(t, rows[d.areas[0].y], "Type something.")
}

func previewQuestion(preview string) []tool.Question {
	q := choice("db", false, "Postgres", "SQLite")
	q.Options[0].Preview = preview
	return []tool.Question{q}
}

// cellOf is the display column of sub in row, -1 when absent.
func cellOf(row, sub string) int {
	i := strings.Index(row, sub)
	if i < 0 {
		return -1
	}
	return ansi.StringWidth(row[:i])
}

func TestQuestionRowsShowThePreviewBesideFromSixtyFourColumns(t *testing.T) {
	qs := previewQuestion("# Heading\n\nsome text")
	rows := plainQuestionRows(qs, 0, 80, 6, 100)
	assert.Equal(t, strings.Repeat("─", 80), rows[0])
	assert.Equal(t, strings.Repeat("─", 80), rows[len(rows)-1])
	first := rows[3]
	assert.True(t, strings.HasPrefix(first, "❭ 1. Postgres"), first)
	// The column is the widest option row plus one, "  3. Type something." and
	// its cursor cell; the box starts previewGap columns after it.
	list := len("  3. Type something.") + 1
	box := cellOf(first, "┌")
	assert.Equal(t, list+previewGap, box)
	assert.Equal(t, box+2, cellOf(rows[4], "Heading"))
	assert.Equal(t, box+2, cellOf(rows[5], "some text"))
	assert.Equal(t, "  enter to choose · tab to add a note · esc to dismiss", rows[len(rows)-2], "the hint spans the whole width")

	assert.Contains(t, plainQuestionRows(qs, 0, 64, 6, 100)[3], "┌", "from 64 columns")
}

func TestQuestionRowsShowThePreviewBelowUnderSixtyFourColumns(t *testing.T) {
	qs := previewQuestion("# Heading\n\nsome text")
	rows := plainQuestionRows(qs, 0, 63, 6, 100)
	rule := strings.Repeat("─", 63)
	box := strings.Repeat("─", 59)
	assert.Equal(t, []string{
		"  3. Type something.",
		"",
		"  ┌" + box + "┐",
		"  │ Heading" + strings.Repeat(" ", 59-8) + "│",
		"  │ some text" + strings.Repeat(" ", 59-10) + "│",
		"  └" + box + "┘",
		"",
		"  enter to choose · tab to add a note · esc to dismiss",
		rule,
	}, rows[7:])
}

func TestQuestionRowsSizePreviewBoxToTheWidestPreviewAndAnchorItLeft(t *testing.T) {
	// "  3. Type something." and its cursor cell make the column 21 wide.
	const list = 21
	for name, tt := range map[string]struct {
		preview string
		want    int
	}{
		"short content": {"ab", minPreviewBox},
		"wider content": {strings.Repeat("w", 20), 24},
		"long content":  {strings.Repeat("w", 100), 80 - list - previewGap},
	} {
		t.Run(name, func(t *testing.T) {
			q := choice("db", false, "A", "B")
			q.Options[0].Preview = tt.preview
			top := plainQuestionRows([]tool.Question{q}, 0, 80, 6, 100)[3]
			start, end := strings.Index(top, "┌"), strings.LastIndex(top, "┐")
			require.GreaterOrEqual(t, start, 0)
			assert.Equal(t, tt.want, ansi.StringWidth(top[start:end+len("┐")]))
			assert.Equal(t, list+previewGap, cellOf(top, "┌"))
		})
	}
}

func TestQuestionRowsKeepThePreviewBoxWidthAsFocusMoves(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	q := choice("db", false, "A", "B")
	q.Options[0].Preview = strings.Repeat("w", 30)
	q.Options[1].Preview = "ab"
	dlg := newQuestionDialog(event.QuestionRequested{Questions: []tool.Question{q}}, &km, mocha())
	edges := func() (int, int) {
		top := plainRows(dlg.dialog(80, mocha()).rows(80, 100, 6, mocha()))[3]
		return cellOf(top, "┌"), cellOf(top, "┐")
	}
	left, right := edges()
	require.GreaterOrEqual(t, left, 0)
	dlg.pages[0].d.move(1)
	l2, r2 := edges()
	assert.Equal(t, left, l2)
	assert.Equal(t, right, r2)
}

func TestQuestionRowsGrowBesideTheListForALongPreview(t *testing.T) {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	qs := previewQuestion(strings.Join(lines, "\n\n"))
	rows := plainQuestionRows(qs, 0, 80, 100, 100)
	assert.Contains(t, rows[len(rows)-5], "line 11", "rows are added before the bottom of the box, the hint and the rule")
	assert.Contains(t, rows[len(rows)-4], "└")
}

func TestQuestionSecondFrameAllocationsDoNotGrowWithThePreview(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool randomly drops items under -race, so allocation counts vary")
	}
	km := newKeyMap(config.DefaultKeys())
	allocs := func(n int) float64 {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = fmt.Sprintf("line %d", i)
		}
		q := newQuestionDialog(event.QuestionRequested{Questions: previewQuestion(strings.Join(lines, "\n\n"))}, &km, mocha())
		q.dialog(80, mocha()).rows(80, 100, 6, mocha())
		return testing.AllocsPerRun(10, func() { q.dialog(80, mocha()).rows(80, 100, 6, mocha()) })
	}
	assert.LessOrEqual(t, allocs(5000), allocs(500)+2)
}

func TestQuestionDialogMeasuresThePreviewsOncePerWidthAndTheme(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	q := newQuestionDialog(event.QuestionRequested{Questions: previewQuestion("one\n\ntwo")}, &km, mocha())
	size := func() [2]int {
		d := q.dialog(80, mocha())
		return [2]int{d.pane.width, d.pane.most}
	}
	measured := size()
	require.Equal(t, [2]int{3, 3}, measured)

	q.sizes[0] = [2]int{7, 9}
	assert.Equal(t, [2]int{7, 9}, size(), "a repeat call reads the kept measure")

	q.dialog(90, mocha())
	assert.Equal(t, measured, size(), "a new width measures again")
}

func TestQuestionDialogRendersTheReviewOncePerWidthThemeAndKey(t *testing.T) {
	m, _ := openQuestion(t, choice("One", false, "A", "B"), choice("Two", false, "C", "D"))
	q := questionOf(m)
	q.tab = 2
	body := func(width int) []string { return q.dialog(width, mocha()).body }
	require.Contains(t, body(70)[0], "1. Which One?")

	q.review.body[0] = "kept"
	assert.Equal(t, "kept", body(70)[0], "a repeat call reads the kept body")
	assert.Contains(t, body(60)[0], "1. Which One?", "a new width renders again")

	q.review.body[0] = "kept"
	press(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	q.tab = 2
	assert.Contains(t, body(60)[0], "1. Which One?", "a key renders again")
}

func TestQuestionRowsCutAPreviewAndScrollIt(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	q := newQuestionDialog(event.QuestionRequested{Questions: previewQuestion(strings.Join(lines, "\n\n"))}, &km, mocha())
	joined := func() string { return strings.Join(plainRows(q.dialog(80, mocha()).rows(80, 100, 6, mocha())), "\n") }

	first := joined()
	assert.Contains(t, first, "line 0")
	assert.NotContains(t, first, "earlier lines")
	assert.Regexp(t, `… \d+ more lines`, first)

	q.current().scroll.off += q.current().scroll.visible
	scrolled := joined()
	assert.Contains(t, scrolled, "… 4 earlier lines")
	assert.NotContains(t, scrolled, "line 0")
	assert.Contains(t, scrolled, "more lines")

	q.current().scroll.off += 100
	end := joined()
	assert.NotContains(t, end, "more lines")
	assert.Contains(t, end, "line 9")
	assert.Contains(t, end, "earlier lines")

	q.pages[0].d.move(1)
	assert.NotContains(t, joined(), "line", "no preview for an option without one")
	q.pages[0].d.move(0)
	assert.NotContains(t, joined(), "earlier lines", "moving the focus resets the scroll")
}

func TestQuestionRowsKeepTheLayoutWhateverIsFocused(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	q := newQuestionDialog(event.QuestionRequested{Questions: previewQuestion("# Heading")}, &km, mocha())
	d := q.pages[0].d
	for _, sel := range []int{0, 1, 2} {
		d.move(sel)
		for _, r := range q.dialog(80, mocha()).rows(80, 100, 6, mocha()) {
			assert.LessOrEqual(t, ansi.StringWidth(r), 80)
		}
		assert.Equal(t, 3, d.optionStarts[0], "the options start on the same row")
	}
}

// validAnswers fails unless the tool accepts answers for qs.
func validAnswers(t *testing.T, qs []tool.Question, answers []tool.Answer) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"questions": qs})
	require.NoError(t, err)
	ask := tool.NewAskUserQuestion(func(context.Context, []tool.Question) ([]tool.Answer, error) { return answers, nil })
	_, err = ask.Run(t.Context(), raw, func(policy.Target) (policy.Mode, error) { return policy.Plan, nil })
	require.NoError(t, err)
}

func openQuestion(t *testing.T, qs ...tool.Question) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, 70, 30)
	send(m, event.QuestionRequested{Token: 7, Questions: qs})
	require.NotNil(t, questionOf(m))
	return m, c
}

// sent is the answers the Controller got; it fails unless it got one set the
// tool accepts.
func sent(t *testing.T, c *fakeController, qs ...tool.Question) []tool.Answer {
	t.Helper()
	require.Len(t, c.questionAnswers, 1)
	assert.Equal(t, uint64(7), c.questionAnswers[0].token)
	validAnswers(t, qs, c.questionAnswers[0].answers)
	return c.questionAnswers[0].answers
}

func typeKeys(m *model, s string) {
	for _, r := range s {
		press(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

var (
	pgDown = tea.KeyPressMsg{Code: tea.KeyPgDown}
	right  = tea.KeyPressMsg{Code: tea.KeyRight}
	space  = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
)

func TestQuestionEnterChoosesTheFocusedOption(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, down)
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}}}, sent(t, c, qs))
	assert.NotNil(t, questionOf(m), "the dialog stays until QuestionEnded")
}

func TestQuestionDigitChoosesAnOption(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, digit('2'))
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}}}, sent(t, c, qs))
}

func TestQuestionTypedAnswer(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, down)
	press(m, down)
	assert.True(t, m.frontDialog().field().ta.Focused())
	typeKeys(m, "mine 2")
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Custom: "mine 2"}}, sent(t, c, qs))
	assert.Equal(t, 1, len(m.geo.fields))
}

func TestQuestionDigitMovesToTheCustomRow(t *testing.T) {
	m, c := openQuestion(t, choice("db", false, "A", "B"))
	press(m, digit('3'))
	assert.True(t, m.frontDialog().field().ta.Focused())
	typeKeys(m, "1")
	assert.Equal(t, "1", m.frontDialog().text(), "digits are text on the custom row")
	assert.Empty(t, c.questionAnswers)
}

func TestQuestionCustomRowGoesBackUp(t *testing.T) {
	m, _ := openQuestion(t, choice("db", false, "A", "B"))
	press(m, digit('3'))
	press(m, up)
	assert.Equal(t, 1, m.frontDialog().sel)
}

func TestQuestionBlankEnterOnTheCustomRowShowsANotice(t *testing.T) {
	m, c := openQuestion(t, choice("db", false, "A", "B"))
	press(m, digit('3'))
	press(m, enter)
	assert.Contains(t, screen(m), "  Choose an option or enter an answer.")
	assert.Empty(t, c.questionAnswers)
	typeKeys(m, "x")
	assert.NotContains(t, strings.Join(screen(m), "\n"), "Choose an option")
}

func TestQuestionTypedAnswerReplacesAChosenOption(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	questionOf(m).pages[0].chosen = 0
	press(m, digit('3'))
	typeKeys(m, "own")
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Custom: "own"}}, sent(t, c, qs))
}

func TestQuestionChosenOptionReplacesATypedAnswer(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, digit('3'))
	typeKeys(m, "own")
	press(m, enter)
	require.Len(t, c.questionAnswers, 1)
	c.questionAnswers = nil
	press(m, up)
	press(m, up)
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 0}}}}, sent(t, c, qs), "the draft text is not sent with a chosen option")
}

func TestQuestionMultiSelectTogglesAndContinues(t *testing.T) {
	qs := []tool.Question{choice("one", true, "A", "B", "C"), choice("two", false, "D", "E")}
	m, c := openQuestion(t, qs...)
	press(m, down)
	press(m, down)
	press(m, space)
	press(m, digit('1'))
	assert.Contains(t, strings.Join(screen(m), "\n"), "[✔] A")
	assert.Contains(t, strings.Join(screen(m), "\n"), "[✔] C")
	press(m, digit('1'))
	press(m, digit('1'))
	assert.Contains(t, strings.Join(screen(m), "\n"), "[✔] A")
	press(m, enter)
	assert.Equal(t, 1, questionOf(m).tab, "enter continues to the next question")
	press(m, digit('2'))
	assert.Equal(t, 2, questionOf(m).tab, "the last question opens the review")
	assert.Empty(t, c.questionAnswers)
	press(m, enter)
	assert.Equal(t, []tool.Answer{
		{Picks: []tool.Pick{{Option: 0}, {Option: 2}}},
		{Picks: []tool.Pick{{Option: 1}}},
	}, sent(t, c, qs...))
}

func TestQuestionMultiSelectEnterNeedsAPickOrText(t *testing.T) {
	qs := choice("one", true, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, enter)
	assert.Contains(t, screen(m), "  Choose an option or enter an answer.")
	assert.Empty(t, c.questionAnswers)

	press(m, digit('3'))
	typeKeys(m, "typed")
	assert.Contains(t, strings.Join(screen(m), "\n"), "[✔] typed")
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Custom: "typed"}}, sent(t, c, qs))
}

func TestQuestionMultiSelectSendsPicksWithTypedText(t *testing.T) {
	qs := choice("one", true, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, digit('2'))
	press(m, digit('3'))
	typeKeys(m, "more")
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}, Custom: "more"}}, sent(t, c, qs))
}

func TestQuestionArrowsSwitchTabsAndKeepDrafts(t *testing.T) {
	qs := []tool.Question{choice("one", true, "A", "B"), choice("two", false, "C", "D")}
	m, _ := openQuestion(t, qs...)
	press(m, digit('2'))
	press(m, down)
	press(m, right)
	assert.Equal(t, 1, questionOf(m).tab)
	press(m, digit('3'))
	typeKeys(m, "draft")
	press(m, left)
	assert.Equal(t, 1, questionOf(m).tab, "left moves the cursor while the draft has text")
	press(m, tea.KeyPressMsg{Code: tea.KeyHome})
	press(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	press(m, right)
	assert.Equal(t, 1, questionOf(m).tab)

	questionOf(m).pages[1].custom().reset()
	press(m, left)
	assert.Equal(t, 0, questionOf(m).tab)
	assert.True(t, questionOf(m).pages[0].picked[1], "the pick survives")
	assert.Equal(t, 1, m.frontDialog().sel, "so does the focused row")
	press(m, right)
	assert.Equal(t, 2, questionOf(m).pages[1].d.sel)
	assert.True(t, m.frontDialog().field().ta.Focused())
}

func TestQuestionReviewJumpsToTheFirstGap(t *testing.T) {
	qs := []tool.Question{choice("one", false, "A", "B"), choice("two", false, "C", "D")}
	m, c := openQuestion(t, qs...)
	press(m, right)
	press(m, digit('1'))
	require.Equal(t, 2, questionOf(m).tab)
	assert.Contains(t, strings.Join(screen(m), "\n"), "↳ Answer required")
	press(m, enter)
	assert.Equal(t, 0, questionOf(m).tab)
	assert.Contains(t, screen(m), "  Answer this question before submitting.")
	assert.Empty(t, c.questionAnswers)

	press(m, digit('2'))
	press(m, enter)
	require.Equal(t, 2, questionOf(m).tab)
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}}, {Picks: []tool.Pick{{Option: 0}}}}, sent(t, c, qs...))
}

func TestQuestionReviewLeftGoesBack(t *testing.T) {
	qs := []tool.Question{choice("one", false, "A", "B"), choice("two", false, "C", "D")}
	m, _ := openQuestion(t, qs...)
	press(m, digit('1'))
	press(m, digit('1'))
	require.Equal(t, 2, questionOf(m).tab)
	press(m, left)
	assert.Equal(t, 1, questionOf(m).tab)
}

func TestQuestionNoteIsSavedAndSentWithThePick(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, tab)
	assert.Contains(t, strings.Join(screen(m), "\n"), "Add a note")
	assert.Contains(t, strings.Join(screen(m), "\n"), "enter to save · esc to go back")
	typeKeys(m, "because 1")
	press(m, enter)
	assert.Nil(t, m.frontDialog().options[0].field)
	assert.Contains(t, strings.Join(screen(m), "\n"), "↳ note: because 1")
	assert.Empty(t, c.questionAnswers, "enter in the editor only saves")

	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 0, Note: "because 1"}}}}, sent(t, c, qs))
}

func TestQuestionNoteEditorTabSavesBlankRemovesAndEscDiscards(t *testing.T) {
	m, c := openQuestion(t, choice("db", false, "A", "B"))
	press(m, tab)
	typeKeys(m, "keep")
	press(m, tab)
	assert.Equal(t, "keep", questionOf(m).pages[0].notes[0])

	press(m, tab)
	assert.Equal(t, "keep", questionOf(m).pages[0].note.value(), "the editor opens on the saved note")
	typeKeys(m, "ing")
	press(m, escape)
	assert.Empty(t, c.questionAnswers, "esc in the editor is not a dismissal")
	assert.Equal(t, "keep", questionOf(m).pages[0].notes[0])
	assert.Equal(t, -1, questionOf(m).pages[0].noting)

	press(m, tab)
	questionOf(m).pages[0].note.reset()
	press(m, enter)
	assert.Empty(t, questionOf(m).pages[0].notes[0])
}

func TestQuestionNoteEditorNeverMovesBetweenOptions(t *testing.T) {
	m, _ := openQuestion(t, choice("db", false, "A", "B"))
	press(m, tab)
	press(m, down)
	press(m, digit('2'))
	assert.Equal(t, 0, m.frontDialog().sel)
	assert.Equal(t, "2", m.frontDialog().text())
}

func TestQuestionNotesOnUnchosenOptionsAreNotSent(t *testing.T) {
	qs := choice("db", true, "A", "B")
	m, c := openQuestion(t, qs)
	press(m, tab)
	typeKeys(m, "no")
	press(m, enter)
	press(m, down)
	press(m, space)
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}}}, sent(t, c, qs))
}

func TestQuestionEscapeDismisses(t *testing.T) {
	for name, setup := range map[string]func(m *model){
		"on an option":      func(*model) {},
		"on the custom row": func(m *model) { press(m, digit('3')) },
		"on the review": func(m *model) {
			press(m, digit('1'))
			press(m, digit('1'))
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, c := openQuestion(t, choice("one", false, "A", "B"), choice("two", false, "C", "D"))
			setup(m)
			press(m, escape)
			require.Len(t, c.questionAnswers, 1)
			assert.Equal(t, questionAnswer{token: 7}, c.questionAnswers[0])
			assert.NotNil(t, questionOf(m))
		})
	}
}

func TestQuestionCancelDismisses(t *testing.T) {
	m, c := openQuestion(t, choice("db", false, "A", "B"))
	press(m, ctrlC)
	assert.Zero(t, c.interrupts)
	assert.Equal(t, []questionAnswer{{token: 7}}, c.questionAnswers)
}

func TestQuestionSwallowsOtherKeys(t *testing.T) {
	m, c := openQuestion(t, choice("db", false, "A", "B"))
	press(m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	press(m, ctrlD)
	press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	assert.Zero(t, c.toggles)
	assert.Empty(t, c.sentNow)
	assert.Empty(t, c.questionAnswers)
	assert.NotNil(t, questionOf(m))
}

func TestQuestionPasteGoesToTheFocusedFieldOnly(t *testing.T) {
	m, _ := openQuestion(t, choice("db", false, "A", "B"))
	typeText(m, "draft")
	pasteText(m, "ignored")
	assert.Equal(t, "draft", m.composer.value())
	assert.Empty(t, questionOf(m).pages[0].customText(), "an option row takes no paste")

	press(m, digit('3'))
	pasteText(m, "pasted")
	assert.Equal(t, "pasted", questionOf(m).pages[0].customText())
	assert.Equal(t, "draft", m.composer.value())

	m.Update(clipboardTextMsg{text: " more", field: m.frontDialog().field()})
	assert.Equal(t, "pasted more", questionOf(m).pages[0].customText())

	m.Update(clipboardTextMsg{text: "stray"})
	assert.Equal(t, "draft", m.composer.value(), "the composer takes no clipboard text meanwhile")
}

func TestQuestionPasteGoesToTheNoteEditor(t *testing.T) {
	m, _ := openQuestion(t, choice("db", false, "A", "B"))
	press(m, tab)
	pasteText(m, "noted")
	assert.Equal(t, "noted", questionOf(m).pages[0].note.value())
}

func TestQuestionKeepsTheComposerDraft(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	typeText(m, "draft")
	send(m, event.QuestionRequested{Token: 7, Questions: []tool.Question{choice("db", false, "A", "B")}})
	assert.NotContains(t, strings.Join(screen(m), "\n"), "draft")
	send(m, event.QuestionEnded{Token: 7})
	assert.Equal(t, "draft", m.composer.value())
	assert.Contains(t, strings.Join(screen(m), "\n"), "draft")
}

func TestQuestionPageKeysScrollACutPreview(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m, _ := newTestModel(t, 90, 20)
	send(m, event.QuestionRequested{Token: 7, Questions: previewQuestion(strings.Join(lines, "\n\n"))})
	assert.Contains(t, strings.Join(screen(m), "\n"), "more lines")
	press(m, pgDown)
	assert.Contains(t, strings.Join(screen(m), "\n"), "earlier lines")
	press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	assert.NotContains(t, strings.Join(screen(m), "\n"), "earlier lines")
}

func TestQuestionFieldAreasMatchTheScreen(t *testing.T) {
	m, _ := openQuestion(t, choice("one", false, "A", "B"), choice("two", false, "C", "D"))
	press(m, digit('3'))
	typeKeys(m, "abc")
	require.Len(t, m.geo.fields, 1)
	fa := m.geo.fields[0]
	assert.Contains(t, screen(m)[fa.y], "abc")
}

func TestQuestionEndsWithItsEventTurnEndOrSessionChange(t *testing.T) {
	qs := []tool.Question{choice("db", false, "A", "B")}
	for _, evs := range [][]event.Event{
		{event.QuestionEnded{Token: 7}},
		{event.QuestionEnded{Token: 7}, event.TurnEnd{}},
		{event.QuestionEnded{Token: 7}, event.SessionChanged{SessionID: "s2"}},
	} {
		m, _ := newTestModel(t, 70, 30)
		send(m, event.QuestionRequested{Token: 7, Questions: qs})
		send(m, evs...)
		assert.Nil(t, questionOf(m), "%T", evs[len(evs)-1])
		assert.Nil(t, m.frontDialog())
		assert.NotContains(t, strings.Join(screen(m), "\n"), "Which db?")
		assert.Contains(t, strings.Join(screen(m), "\n"), "❯")
	}

	m, _ := newTestModel(t, 70, 30)
	send(m, event.QuestionRequested{Token: 7, Questions: qs})
	send(m, event.QuestionEnded{Token: 8})
	assert.NotNil(t, questionOf(m), "another token's end is ignored")
}

func TestQuestionRequestCoversTheTasksDialog(t *testing.T) {
	m, _ := tasksModel(t)
	submitText(m, "/tasks")
	require.NotNil(t, tasksOf(m))
	send(m, event.QuestionRequested{Token: 7, Questions: []tool.Question{choice("db", false, "A", "B")}})
	assert.NotNil(t, tasksOf(m))
	assert.Equal(t, promptQuestion, frontKind(m))

	send(m, event.QuestionEnded{Token: 7})
	assert.Equal(t, promptTasks, frontKind(m))
}

func TestQuestionReturnsAfterAChildApprovalOverIt(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := openQuestion(t, qs)
	send(m, subagentAsk(2, "explore"))
	text := strings.Join(screen(m), "\n")
	assert.Contains(t, text, "explore t2 › Edit explore.go")
	assert.NotContains(t, text, "Which db?")

	press(m, enter)
	assert.Equal(t, []answer{{2, policy.Decision{Answer: policy.Allow}}}, c.answers)
	assert.Empty(t, c.questionAnswers, "keys go to the approval")

	send(m, event.ApprovalEnded{Token: 2})
	assert.Nil(t, approvalOf(m))
	assert.Contains(t, strings.Join(screen(m), "\n"), "Which db?")
	press(m, enter)
	assert.Equal(t, []tool.Answer{{Picks: []tool.Pick{{Option: 0}}}}, sent(t, c, qs))
}

func TestQuestionWaitsForAChildApprovalAlreadyShown(t *testing.T) {
	qs := choice("db", false, "A", "B")
	m, c := newTestModel(t, 70, 30)
	send(m, subagentAsk(2, "explore"))
	send(m, event.QuestionRequested{Token: 7, Questions: []tool.Question{qs}})
	require.NotNil(t, questionOf(m))
	text := strings.Join(screen(m), "\n")
	assert.Contains(t, text, "explore t2 › Edit explore.go")
	assert.NotContains(t, text, "Which db?")

	press(m, enter)
	assert.Len(t, c.answers, 1)
	assert.Empty(t, c.questionAnswers)

	send(m, event.ApprovalEnded{Token: 2})
	assert.Contains(t, strings.Join(screen(m), "\n"), "Which db?")
	press(m, enter)
	sent(t, c, qs)
}

func TestQuestionResizeKeepsTheBottomRuleLast(t *testing.T) {
	m, _ := openQuestion(t, choice("one", false, "A", "B", "C", "D"), choice("two", false, "C", "D"))
	press(m, digit('5'))
	typeKeys(m, "x")
	for _, height := range []int{8, 5, 30, 6} {
		m.Update(tea.WindowSizeMsg{Width: 40, Height: height})
		rows := screen(m)
		require.Len(t, rows, height)
		assert.Equal(t, strings.Repeat("─", 40), rows[height-1])
		if height >= 8 {
			assert.Contains(t, strings.Join(rows, "\n"), "5. x")
		}
	}
}

func TestQuestionRowsHintScrollingOnlyWhenSomethingIsCut(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	q := newQuestionDialog(event.QuestionRequested{Questions: previewQuestion(strings.Join(lines, "\n\n"))}, &km, mocha())
	rows := plainRows(q.dialog(80, mocha()).rows(80, 100, 6, mocha()))
	assert.Equal(t, "  pgup/pgdown to scroll · enter to choose · tab to add a note · esc to dismiss", rows[len(rows)-2])
	assert.Regexp(t, `│ … \d+ more lines +│$`, rows[len(rows)-5], "markers sit inside the box")

	rows = plainRows(q.dialog(80, mocha()).rows(80, 100, 100, mocha()))
	assert.Equal(t, "  enter to choose · tab to add a note · esc to dismiss", rows[len(rows)-2])
}

func TestQuestionRowsMarkTheChosenOptionAndCustomAnswer(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	th := mocha()
	q := newQuestionDialog(event.QuestionRequested{Questions: []tool.Question{choice("db", false, "A", "B")}}, &km, th)
	p := q.pages[0]
	p.chosen = 0
	rows := q.dialog(60, th).rows(60, 100, 6, th)
	assert.Equal(t, "❭ 1. A ✔", plainRows(rows)[3])
	assert.Equal(t, fg(th.accent).Render("❭ ")+fg(th.accent).Bold(true).Render("1. A ✔"), rows[3], "chosen and focused: bold accent")
	assert.Equal(t, fg(th.accent).Render("  ")+fg(th.text).Render("2. B"), rows[5], "an unchosen option is in the text colour")

	p.custom().ta.SetValue("mine")
	p.chosen = 2
	p.d.move(0)
	rows = plainRows(q.dialog(60, th).rows(60, 100, 6, th))
	assert.Equal(t, "  3. mine ✔", rows[7])
}

func TestQuestionRowsDrawTheNoteEditorBelowTheDetail(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	q := newQuestionDialog(event.QuestionRequested{Questions: []tool.Question{choice("db", false, "A", "B")}}, &km, mocha())
	p := q.pages[0]
	p.notes[0] = "kept"
	assert.Equal(t, []string{"❭ 1. A", "  about A", "  ↳ note: kept", "  2. B"}, plainRows(q.dialog(60, mocha()).rows(60, 100, 6, mocha()))[3:7])

	p.openNote()
	p.note.ta.SetValue("a long note that goes on and on past the end of the row so it wraps")
	rows := plainRows(q.dialog(40, mocha()).rows(40, 100, 6, mocha()))
	assert.Equal(t, []string{"❭ 1. A", "  about A", "  ↳ note: a long note that goes on and"}, rows[3:6])
	assert.True(t, strings.HasPrefix(rows[6], "          "), "continuation rows hang under the text: %q", rows[6])
}

func TestQuestionPageKeysScrollACutPreviewFromTheNoteEditor(t *testing.T) {
	var lines []string
	for i := range 60 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m, _ := openQuestion(t, previewQuestion(strings.Join(lines, "\n"))...)
	press(m, tab)
	typeKeys(m, "abc")
	require.True(t, questionOf(m).current().scroll.cut())

	press(m, pgDown)

	assert.Positive(t, questionOf(m).current().scroll.off)
	assert.Equal(t, "abc", questionOf(m).pages[0].note.value())
}
