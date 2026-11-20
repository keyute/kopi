package sandbox

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// handoffFD is the helper's end of the handoff socket: the third extra
	// file of the bwrap command.
	handoffFD = 5
	// handoffTimeout bounds each side's wait for the other, as in OpenAI Codex's
	// proxy routing, whose design this follows.
	handoffTimeout = 30 * time.Second
)

// ExecListening runs inside a sandboxed command's network namespace. It
// listens on the namespace's loopback port, gives Kopi that listener, waits
// until Kopi serves it and then becomes the command argv.
func ExecListening(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("failed to find %s: %w", argv[0], err)
	}
	l, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to create the listener: %w", err)
	}
	if err := unix.Bind(l, &unix.SockaddrInet4{Port: listenPort, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return fmt.Errorf("failed to bind 127.0.0.1:%d: %w", listenPort, err)
	}
	if err := unix.Listen(l, unix.SOMAXCONN); err != nil {
		return fmt.Errorf("failed to listen on 127.0.0.1:%d: %w", listenPort, err)
	}
	// One byte of data carries the descriptor on a stream socket.
	for {
		err = unix.Sendmsg(handoffFD, []byte{0}, unix.UnixRights(l), nil, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("failed to hand the listener to Kopi: %w", err)
	}
	tv := unix.NsecToTimeval(handoffTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(handoffFD, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("failed to set the handoff timeout: %w", err)
	}
	var ack [1]byte
	var n int
	for {
		n, err = unix.Read(handoffFD, ack[:])
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("failed to wait for Kopi to serve the listener: %w", err)
	}
	if n == 0 {
		return errors.New("failed to wait for Kopi to serve the listener: Kopi closed the handoff")
	}
	// Kopi holds its own copy of the listener; the command must inherit
	// neither descriptor.
	_ = unix.Close(handoffFD)
	_ = unix.Close(l)
	if err := unix.Exec(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("failed to run %s: %w", path, err)
	}
	return nil
}

// handoff receives the listener the helper binds in a command's network
// namespace and serves it.
type handoff struct {
	child *os.File // the helper's end, for the command's extra files
	host  *net.UnixConn
	done  chan struct{} // closed when the receiving goroutine has ended
	// session is set by the goroutine before done is closed, and only if it
	// received a listener.
	session *session
}

// handoff starts waiting for a helper's listener. Closing child, here and in
// every process that inherited it, ends the wait.
func (p *Proxy) handoff() (*handoff, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create the handoff socket: %w", err)
	}
	child, hostFile := os.NewFile(uintptr(fds[1]), "handoff"), os.NewFile(uintptr(fds[0]), "handoff")
	conn, err := net.FileConn(hostFile)
	_ = hostFile.Close()
	if err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("failed to use the handoff socket: %w", err)
	}
	h := &handoff{child: child, host: conn.(*net.UnixConn), done: make(chan struct{})}
	go h.receive(p)
	return h, nil
}

func (h *handoff) receive(p *Proxy) {
	defer close(h.done)
	_ = h.host.SetReadDeadline(time.Now().Add(handoffTimeout))
	l, err := receiveListener(h.host)
	if err != nil {
		slog.Warn("failed to receive the sandbox proxy listener", "error", err)
		return
	}
	h.session = p.serve(l)
	if _, err := h.host.Write([]byte{0}); err != nil {
		slog.Warn("failed to acknowledge the sandbox proxy listener", "error", err)
	}
}

// receiveListener reads one message holding exactly one descriptor, a TCP
// listener.
func receiveListener(conn *net.UnixConn) (net.Listener, error) {
	var data [1]byte
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, flags, _, err := conn.ReadMsgUnix(data[:], oob)
	// Wrapped at once so that whatever arrived is closed, however it is used.
	var files []*os.File
	if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
		for i := range msgs {
			fds, perr := unix.ParseUnixRights(&msgs[i])
			if perr != nil {
				continue
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "listener"))
			}
		}
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("failed to read the handoff: %w", err)
	}
	if flags&unix.MSG_CTRUNC != 0 || len(files) != 1 {
		return nil, fmt.Errorf("failed to read the handoff: expected one descriptor, got %d", len(files))
	}
	l, err := net.FileListener(files[0])
	if err != nil {
		return nil, fmt.Errorf("failed to use the received descriptor: %w", err)
	}
	if _, ok := l.(*net.TCPListener); !ok {
		_ = l.Close()
		return nil, errors.New("failed to use the received descriptor: not a TCP listener")
	}
	return l, nil
}

// close stops waiting for the helper, closes the session if one was started
// and returns the destinations it refused. The caller has closed child.
func (h *handoff) close() []string {
	_ = h.host.Close()
	<-h.done
	if h.session == nil {
		return nil
	}
	return h.session.close()
}
