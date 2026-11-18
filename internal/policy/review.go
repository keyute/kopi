package policy

import "context"

type Verdict int

const (
	// VerdictAsk is undecided. It is the zero value, so a bad value falls to
	// the human.
	VerdictAsk Verdict = iota
	VerdictAllow
	VerdictDeny
)

// Review is a Reviewer's answer about a call.
type Review struct {
	Verdict Verdict
	Reason  string
}

// Reviewer decides a call that no rule matched and that would otherwise ask.
// It must return once ctx is done; an error counts as VerdictAsk.
type Reviewer func(ctx context.Context, r Request) (Review, error)

const (
	// maxConsecutiveDenials and maxTotalDenials are how many reviewer denials
	// the gate takes before it hands the call to the human instead.
	maxConsecutiveDenials = 3
	maxTotalDenials       = 20
)
