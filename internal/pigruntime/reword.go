package pigruntime

import "strings"

// pigTexts rewrites the PiG v0.8.0 messages that tell the user to run another
// tool's command. Recheck them on a PiG bump: a reworded upstream message
// shows through unchanged.
var pigTexts = strings.NewReplacer(
	"run `pig login github-copilot`", "run `kopi login github-copilot`",
	"Run `/login meta`", "Run `kopi login meta`",
	"another pig session", "another Kopi session",
	"Run '/login ", "Run 'kopi login ",
	"Use /login to log into a provider", "Use `kopi login <provider>` to log into a provider",
)

// rewordedError shows err's text through pigTexts and keeps err for
// errors.Is and errors.As.
type rewordedError struct{ err error }

func (e rewordedError) Error() string { return pigTexts.Replace(e.err.Error()) }

func (e rewordedError) Unwrap() error { return e.err }

func reword(err error) error { return rewordedError{err: err} }
