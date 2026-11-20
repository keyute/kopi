package pigruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/MichaelKinsy/PiG/codemode"
)

// scriptHeapBytes bounds one script's VM heap.
const scriptHeapBytes = 128 << 20

// ScriptFunc answers one host(arg) call of a script. ctx is cancelled when
// the script's run ends.
type ScriptFunc func(ctx context.Context, arg json.RawMessage) (json.RawMessage, error)

// RunScript runs code, the body of an async JavaScript function, in a VM of
// its own. host(arg) in the script returns a promise of call's result, and
// calls may run concurrently. It returns the JSON of the returned value; a
// throw wraps ErrScript with the stack, whose line numbers are code's, and
// any other VM failure returns a plain error;
// cancelling ctx cancels every call and returns ctx's cause. It returns once
// every call has returned and the VM is gone.
func RunScript(ctx context.Context, code string, call ScriptFunc) (json.RawMessage, error) {
	var exited <-chan struct{}
	sb, err := codemode.NewSandbox(codemode.SandboxOptions{
		Globals:          []codemode.Tool{{Name: "host", Execute: call}},
		MemoryLimitBytes: scriptHeapBytes,
		// PiG treats a zero TimeoutMs as 300 s; a workflow has no time limit.
		TimeoutMs: math.Inf(1),
		OnWorkerStarted: func(ch <-chan struct{}) error {
			exited = ch
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create script sandbox: %w", err)
	}
	defer func() { _ = sb.Close() }()
	res, err := sb.Execute(ctx, code, codemode.ExecuteOptions{})
	// PiG's Execute returns before the VM goroutine has exited; the VM is
	// halted within codemode's grace period, so this wait is bounded.
	if exited != nil {
		<-exited
	}
	if err != nil {
		return nil, fmt.Errorf("failed to run script: %w", err)
	}
	if res.OK {
		return res.Value, nil
	}
	if res.Error.Kind == codemode.ErrorAborted && ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if res.Error.Kind != codemode.ErrorScript {
		return nil, fmt.Errorf("failed to run script: %s", res.Error.Message)
	}
	return nil, fmt.Errorf("%w: %s: %s\n%s", ErrScript, res.Error.Name, res.Error.Message, res.Error.Stack)
}
