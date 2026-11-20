package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/keyute/kopi/internal/policy"
)

const questionsDismissed = "The user dismissed the questions without answering."

// Question is one question put to the human.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
}

// Answer is one question's answer: the chosen options in option order, and/or typed text.
type Answer struct {
	Picks  []Pick
	Custom string
}

type Pick struct {
	Option int // index into Question.Options
	Note   string
}

// Ask shows qs to the human; nil answers with a nil error means they dismissed them.
type Ask func(ctx context.Context, qs []Question) ([]Answer, error)

type askUserQuestionArgs struct {
	Questions []Question `json:"questions"`
}

// NewAskUserQuestion returns the tool that puts questions to the human through ask.
func NewAskUserQuestion(ask Ask) Tool {
	return Tool{
		Name:  "AskUserQuestion",
		Label: "AskUserQuestion",
		Description: "Asks the user one to four questions and waits for the answers. " +
			"Each question offers two to four options, and the user can always type an answer of their own instead, so an option labelled \"Other\" is refused. " +
			"The user may add a note to an option they choose, or dismiss the questions without answering; the result says which.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": 4,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"question": map[string]any{
								"type":        "string",
								"description": "The complete question.",
							},
							"header": map[string]any{
								"type":        "string",
								"description": "A short label for the question, shown as its tab when there are several; at most 12 characters.",
							},
							"options": map[string]any{
								"type":     "array",
								"minItems": 2,
								"maxItems": 4,
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label": map[string]any{
											"type":        "string",
											"description": "The choice.",
										},
										"description": map[string]any{
											"type":        "string",
											"description": "What the choice means.",
										},
										"preview": map[string]any{
											"type":        "string",
											"description": "Markdown shown while this option is focused; single-select questions only.",
										},
									},
									"required":             []string{"label", "description"},
									"additionalProperties": false,
								},
							},
							"multiSelect": map[string]any{
								"type":        "boolean",
								"description": "Whether the user may choose several options.",
							},
						},
						"required":             []string{"question", "header", "options"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"questions"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Exclusive:  true,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[askUserQuestionArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if err := checkQuestions(args.Questions); err != nil {
				return Result{}, err
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			answers, err := ask(ctx, args.Questions)
			if err != nil {
				return Result{}, err
			}
			if answers == nil {
				return Result{Text: questionsDismissed}, nil
			}
			if err := checkAnswers(args.Questions, answers); err != nil {
				return Result{}, fmt.Errorf("failed to read the answers: %w", err)
			}
			return Result{Text: formatAnswers(args.Questions, answers)}, nil
		},
		Display: Display{
			Title: func(raw json.RawMessage) string {
				var args askUserQuestionArgs
				if json.Unmarshal(raw, &args) != nil || len(args.Questions) == 0 {
					return "AskUserQuestion"
				}
				headers := make([]string, len(args.Questions))
				for i, q := range args.Questions {
					headers[i] = q.Header
				}
				return titleLine("Asked about " + strings.Join(headers, ", "))
			},
			Summary: func(output string) string {
				if output == questionsDismissed {
					return "dismissed"
				}
				_, lines, _ := strings.Cut(output, "\n")
				var answers []string
				for _, line := range strings.Split(lines, "\n") {
					if q, err := strconv.QuotedPrefix(line); err == nil {
						line = line[len(q):]
					}
					answers = append(answers, strings.TrimPrefix(line, " = "))
				}
				return strings.Join(answers, " · ")
			},
			Solo: true,
			Body: func(string) string { return "" },
		},
	}
}

func checkQuestions(qs []Question) error {
	if len(qs) < 1 || len(qs) > 4 {
		return fmt.Errorf("%w: ask one to four questions, got %d", ErrInvalidArgs, len(qs))
	}
	for i, q := range qs {
		if err := checkQuestion(q); err != nil {
			return fmt.Errorf("%w: question %d: %w", ErrInvalidArgs, i+1, err)
		}
	}
	return nil
}

func checkQuestion(q Question) error {
	if strings.TrimSpace(q.Question) == "" {
		return fmt.Errorf("question is blank")
	}
	if strings.TrimSpace(q.Header) == "" {
		return fmt.Errorf("header is blank")
	}
	if strings.ContainsAny(q.Header, "\r\n\t") {
		return fmt.Errorf("header must not contain a line break or tab")
	}
	if len(q.Options) < 2 || len(q.Options) > 4 {
		return fmt.Errorf("offer two to four options, got %d", len(q.Options))
	}
	seen := make(map[string]bool, len(q.Options))
	for j, o := range q.Options {
		label := strings.ToLower(strings.TrimSpace(o.Label))
		switch {
		case label == "":
			return fmt.Errorf("option %d: label is blank", j+1)
		case strings.ContainsAny(o.Label, "\r\n\t"):
			return fmt.Errorf("option %d: label must not contain a line break or tab", j+1)
		case label == "other":
			return fmt.Errorf("option %d: label %q is reserved: the user can always type their own answer", j+1, o.Label)
		case seen[label]:
			return fmt.Errorf("option %d: label %q repeats another option", j+1, o.Label)
		case o.Preview != "" && q.MultiSelect:
			return fmt.Errorf("option %d: preview is for single-select questions only", j+1)
		}
		seen[label] = true
		for _, s := range []string{o.Label, o.Description, o.Preview} {
			if hasControl(s) {
				return fmt.Errorf("option %d: contains a control character", j+1)
			}
		}
	}
	if hasControl(q.Question) || hasControl(q.Header) {
		return fmt.Errorf("contains a control character")
	}
	return nil
}

// hasControl reports a terminal control character other than newline and tab.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	})
}

func checkAnswers(qs []Question, answers []Answer) error {
	if len(answers) != len(qs) {
		return fmt.Errorf("got %d answers for %d questions", len(answers), len(qs))
	}
	for i, a := range answers {
		q := qs[i]
		for j, p := range a.Picks {
			if p.Option < 0 || p.Option >= len(q.Options) {
				return fmt.Errorf("question %d: option %d is out of range", i+1, p.Option)
			}
			if j > 0 && p.Option <= a.Picks[j-1].Option {
				return fmt.Errorf("question %d: picks are not in option order", i+1)
			}
		}
		typed := strings.TrimSpace(a.Custom) != ""
		switch {
		case q.MultiSelect && len(a.Picks) == 0 && !typed:
			return fmt.Errorf("question %d: no answer", i+1)
		case !q.MultiSelect && len(a.Picks) == 1 && typed:
			return fmt.Errorf("question %d: both an option and typed text", i+1)
		case !q.MultiSelect && len(a.Picks) > 1:
			return fmt.Errorf("question %d: several options for a single-select question", i+1)
		case !q.MultiSelect && len(a.Picks) == 0 && !typed:
			return fmt.Errorf("question %d: no answer", i+1)
		}
	}
	return nil
}

func formatAnswers(qs []Question, answers []Answer) string {
	var b strings.Builder
	b.WriteString("The user answered:")
	for i, a := range answers {
		var parts []string
		for _, p := range a.Picks {
			part := strconv.Quote(qs[i].Options[p.Option].Label)
			if note := strings.TrimSpace(p.Note); note != "" {
				part += " with note " + strconv.Quote(note)
			}
			parts = append(parts, part)
		}
		if custom := strings.TrimSpace(a.Custom); custom != "" {
			parts = append(parts, "typed "+strconv.Quote(custom))
		}
		b.WriteString("\n" + strconv.Quote(qs[i].Question) + " = " + strings.Join(parts, ", "))
	}
	return b.String()
}
