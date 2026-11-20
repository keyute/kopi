package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func echoArg(_ context.Context, arg json.RawMessage) (json.RawMessage, error) { return arg, nil }

func TestRunScriptReturnsValue(t *testing.T) {
	out, err := RunScript(t.Context(), `return (await host({n: 2})).n + 1`, echoArg)
	require.NoError(t, err)
	assert.JSONEq(t, `3`, string(out))
}

func TestRunScriptHostCallsRunTogether(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	call := func(ctx context.Context, arg json.RawMessage) (json.RawMessage, error) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return arg, nil
	}
	done := make(chan json.RawMessage, 1)
	go func() {
		out, err := RunScript(t.Context(), `return await Promise.all([host(1), host(2)])`, call)
		assert.NoError(t, err)
		done <- out
	}()
	<-arrived
	<-arrived
	close(release)
	assert.JSONEq(t, `[1,2]`, string(<-done))
}

func TestRunScriptHostErrorIsCatchable(t *testing.T) {
	call := func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("no such agent")
	}
	out, err := RunScript(t.Context(), `try { await host(1) } catch (e) { return e.message }`, call)
	require.NoError(t, err)
	assert.JSONEq(t, `"no such agent"`, string(out))
}

func TestRunScriptThrowReportsLineOfCode(t *testing.T) {
	_, err := RunScript(t.Context(), "const a = 1\nconst b = 2\nthrow new Error('boom')", echoArg)
	require.ErrorIs(t, err, ErrScript)
	assert.Contains(t, err.Error(), "boom")
	assert.Contains(t, err.Error(), ":3")
}

func TestRunScriptCancelDuringBlockedCall(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	cause := errors.New("stopped")
	ctx, cancel := context.WithCancelCause(t.Context())
	started := make(chan struct{})
	call := func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		<-started
		cancel(cause)
	}()
	_, err := RunScript(ctx, `await host(1)`, call)
	assert.ErrorIs(t, err, cause)
	goleak.VerifyNone(t, ignore)
}

func TestRunScriptCancelDuringBusyLoop(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	call := func(context.Context, json.RawMessage) (json.RawMessage, error) {
		close(started)
		return nil, nil
	}
	go func() {
		<-started
		cancel()
	}()
	_, err := RunScript(ctx, `await host(1); while (true) {}`, call)
	assert.ErrorIs(t, err, context.Canceled)
	goleak.VerifyNone(t, ignore)
}

func TestRunScriptAllocationLoopFails(t *testing.T) {
	_, err := RunScript(t.Context(), `const a = []; while (true) a.push(new Array(1 << 20).fill(1))`, echoArg)
	require.ErrorIs(t, err, ErrScript)
}

func TestRunScriptUnsettledPromiseFails(t *testing.T) {
	_, err := RunScript(t.Context(), `await new Promise(() => {})`, echoArg)
	require.ErrorIs(t, err, ErrScript)
}
