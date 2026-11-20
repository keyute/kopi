package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const (
	planApproved  = "The user approved the plan."
	planRevise    = "The user asked for changes to the plan."
	planDismissed = "The user dismissed the plan without answering."
	planEdited    = " The user edited the plan file before answering."
	feedbackLead  = " Their feedback:\n"
)

// PlanAnswer is the human's answer to a plan. Not approved with blank Feedback
// is a dismissal.
type PlanAnswer struct {
	Approved bool
	Feedback string
}

// ReviewPlan shows plan to the user and waits. The mode is the session's mode
// after the answer was applied.
type ReviewPlan func(ctx context.Context, plan string) (PlanAnswer, policy.Mode, error)

// NewExitPlanMode returns the tool that puts the plan in plans' file before the
// user through review.
func NewExitPlanMode(plans *Plans, review ReviewPlan) Tool {
	return Tool{
		Name:  "ExitPlanMode",
		Label: "ExitPlanMode",
		Description: "Puts the plan in the session's plan file before the user and waits for their answer. " +
			"It takes no arguments: it reads the plan from that file, which the plan-mode message names. " +
			"The user approves it, which switches the session to execute mode, asks for changes with feedback, or dismisses it; the result says which, and whether the user edited the file. " +
			"It works only in plan mode.",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Exclusive:  true,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			if _, err := decodeArgs[struct{}](raw); err != nil {
				return Result{}, err
			}
			mode, err := admit(policy.Target{})
			if err != nil {
				return Result{}, err
			}
			if mode != policy.Plan {
				return Result{}, ErrNotPlanMode
			}
			plan, err := plans.Read()
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return Result{}, err
			}
			if strings.TrimSpace(plan) == "" {
				if path := plans.Path(); path != "" {
					return Result{}, fmt.Errorf("%w: write the plan to %s first", ErrNoPlan, path)
				}
				return Result{}, fmt.Errorf("%w: this session has no plan file", ErrNoPlan)
			}
			answer, mode, err := review(ctx, plan)
			if err != nil {
				return Result{}, err
			}
			after, err := plans.Read()
			return Result{Text: planResult(answer, mode, err != nil || after != plan)}, nil
		},
		Display: Display{
			Title: func(json.RawMessage) string { return "Plan approval" },
			Solo:  true,
			Body: func(output string) string {
				if !strings.HasPrefix(output, planRevise) {
					return ""
				}
				_, feedback, _ := strings.Cut(output, feedbackLead)
				return feedback
			},
			Summary: func(output string) string {
				switch {
				case strings.HasPrefix(output, planApproved):
					return "approved"
				case strings.HasPrefix(output, planRevise):
					return "revision requested"
				case strings.HasPrefix(output, planDismissed):
					return "dismissed"
				}
				return ""
			},
		},
	}
}

func planResult(a PlanAnswer, mode policy.Mode, edited bool) string {
	var text string
	switch {
	case a.Approved:
		text = planApproved
	case a.Feedback != "":
		text = planRevise
	default:
		text = planDismissed
	}
	if edited {
		text += planEdited
	}
	text += fmt.Sprintf(" The session is in %s mode.", mode)
	if !a.Approved && a.Feedback != "" {
		text += feedbackLead + a.Feedback
	}
	return text
}
