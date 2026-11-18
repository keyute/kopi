package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PROC_THREAD_ATTRIBUTE_JOB_LIST requires Windows 10 / Server 2016.
// x/sys does not yet name this SDK constant. Assignment is part of creation,
// before any child code runs; launch-then-assign cannot safely own descendants.
const statusJobList = 0x0002000d

func (shellRunner) run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	if cmd.Err != nil {
		return "", fmt.Errorf("failed to resolve status shell: %w", cmd.Err)
	}
	return runWindowsStatus(ctx, cmd.Path, cmd.Args, cmd.Environ(), dir)
}

func runWindowsStatus(ctx context.Context, executable string, args, env []string, dir string) (outputText string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	app, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return "", fmt.Errorf("failed to encode status executable: %w", err)
	}
	escaped := make([]string, len(args))
	for i, arg := range args {
		escaped[i] = syscall.EscapeArg(arg)
	}
	command, err := windows.UTF16PtrFromString(strings.Join(escaped, " "))
	if err != nil {
		return "", fmt.Errorf("failed to encode status arguments: %w", err)
	}
	var cwd *uint16
	if dir != "" {
		cwd, err = windows.UTF16PtrFromString(dir)
		if err != nil {
			return "", fmt.Errorf("failed to encode status workspace: %w", err)
		}
	}
	env = append([]string(nil), env...)
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	for _, value := range env {
		if strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("failed to encode status environment: %w", syscall.EINVAL)
		}
	}
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create status job: %w", err)
	}
	defer func() {
		if job != 0 {
			if err := windows.CloseHandle(job); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("failed to close status job: %w", err))
			}
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return "", fmt.Errorf("failed to configure status job: %w", err)
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return "", fmt.Errorf("failed to create status process attributes: %w", err)
	}
	defer attrs.Delete()
	if err := attrs.Update(statusJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return "", fmt.Errorf("failed to set status job attribute: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("failed to create status output pipe: %w", err)
	}
	defer r.Close()
	defer w.Close()
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	null, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "", fmt.Errorf("failed to open status null handle: %w", err)
	}
	defer func() {
		if err := windows.CloseHandle(null); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("failed to close status null handle: %w", err))
		}
	}()
	output := windows.Handle(w.Fd())
	if err := windows.SetHandleInformation(output, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return "", fmt.Errorf("failed to make status output inheritable: %w", err)
	}
	handles := []windows.Handle{null, output}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return "", fmt.Errorf("failed to set status handle attribute: %w", err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = null, output, null
	var pi windows.ProcessInformation
	if err := ctx.Err(); err != nil {
		return "", err
	}
	err = windows.CreateProcess(app, command, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, &block[0], cwd, &si.StartupInfo, &pi)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(job)
	if err != nil {
		return "", fmt.Errorf("failed to run status command: %w", err)
	}
	defer func() {
		if err := windows.CloseHandle(pi.Process); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("failed to close status process handle: %w", err))
		}
	}()
	defer func() {
		if err := windows.CloseHandle(pi.Thread); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("failed to close status thread handle: %w", err))
		}
	}()
	_ = w.Close()
	var out bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&out, r); close(drained) }()
	exited := make(chan error, 1)
	stopWait := make(chan struct{})
	go func() {
		for {
			state, err := windows.WaitForSingleObject(pi.Process, 1000)
			if err != nil || state == windows.WAIT_OBJECT_0 {
				exited <- err
				return
			}
			select {
			case <-stopWait:
				exited <- exec.ErrWaitDelay
				return
			default:
			}
		}
	}()
	var reason error
	observed := false
	select {
	case reason = <-exited:
		observed = true
	case <-ctx.Done():
		reason = ctx.Err()
	}
	// One owner tears down the whole job, including on normal shell exit.
	// Waiting for output first would hang when a descendant retains stdout.
	killErr := windows.TerminateJobObject(job, 1)
	closeErr := windows.CloseHandle(job)
	job = 0
	if killErr != nil {
		reason = errors.Join(reason, fmt.Errorf("failed to terminate status job: %w", killErr))
	}
	if closeErr != nil {
		reason = errors.Join(reason, fmt.Errorf("failed to close status job: %w", closeErr))
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		_ = r.Close()
		<-drained
		reason = errors.Join(reason, exec.ErrWaitDelay)
	}
	// Process handles remain valid independently of job teardown and PID reuse.
	if !observed {
		close(stopWait)
		if err := <-exited; err != nil {
			reason = errors.Join(reason, fmt.Errorf("failed to wait for status command: %w", err))
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		reason = errors.Join(reason, fmt.Errorf("failed to read status exit code: %w", err))
	} else if code != 0 && reason == nil {
		reason = fmt.Errorf("failed to run status command: exit code %d", code)
	}
	if reason != nil {
		return "", reason
	}
	return out.String(), nil
}
