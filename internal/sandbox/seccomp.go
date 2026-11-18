package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// Linux's values on every architecture Kopi supports.
const (
	afUnix       = 1
	afVsock      = 40
	cloneNewUser = 0x10000000
	enosys       = 38
	sockDgram    = 2   // bit set in SOCK_DGRAM (2) and SOCK_RAW (3)
	sockNotDgram = 0xc // bits clear in both, within SOCK_TYPE_MASK
)

// seccompFilter returns the program bwrap's --seccomp installs, as
// little-endian sock_filter records. These fail with EPERM: creating a Unix
// or VM socket or a Unix datagram socketpair, which the network namespace
// does not fence; using io_uring,
// whose operations create sockets without socket(2); and creating a user
// namespace, in which a command could detach the mounts that deny it paths.
// clone3 fails with ENOSYS, as its flags lie in memory a filter cannot read,
// and libc then falls back to clone. Everything else is allowed. The rules
// name native syscall numbers, so a call through another ABI, such as int
// 0x80 on amd64, kills the process; x32 calls get ENOSYS from the library's
// own check.
func seccompFilter() ([]byte, error) {
	info, err := arch.GetInfo("")
	if err != nil {
		return nil, fmt.Errorf("failed to build the seccomp filter: %w", err)
	}
	newUser := seccomp.ArgumentConditions{{Argument: 0, Operation: seccomp.BitsSet, Value: cloneNewUser}}
	p := seccomp.Policy{
		DefaultAction: seccomp.ActionAllow,
		Syscalls: []seccomp.SyscallGroup{
			{
				NamesWithCondtions: []seccomp.NameWithConditions{
					{Name: "socket", Conditions: lowWordEquals(0, afUnix)},
					// A datagram pair can sendto any path socket; stream and
					// seqpacket pairs cannot address anything else. Linux makes
					// SOCK_RAW (3) a datagram too, so the type's low four bits
					// are 2 or 3, whatever flags ride above.
					{Name: "socketpair", Conditions: append(lowWordEquals(0, afUnix), seccomp.ArgumentConditions{
						{Argument: 1, Operation: seccomp.BitsSet, Value: sockDgram},
						{Argument: 1, Operation: seccomp.BitsNotSet, Value: sockNotDgram},
					}...)},
					{Name: "socket", Conditions: lowWordEquals(0, afVsock)},
					// The flags are the first argument of both on amd64 and
					// arm64.
					{Name: "unshare", Conditions: newUser},
					{Name: "clone", Conditions: newUser},
				},
				Action: seccomp.ActionErrno,
			},
			{Names: []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}, Action: seccomp.ActionErrno},
			// The assembler adds EPERM only to a bare ActionErrno.
			{Names: []string{"clone3"}, Action: seccomp.ActionErrno | enosys},
		},
	}
	prog, err := p.Assemble()
	if err != nil {
		return nil, fmt.Errorf("failed to build the seccomp filter: %w", err)
	}
	guard := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 4, Size: 4}, // seccomp_data.arch
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(info.ID), SkipTrue: 1},
		bpf.RetConstant{Val: uint32(seccomp.ActionKillProcess)},
	}
	raw, err := bpf.Assemble(append(guard, prog...))
	if err != nil {
		return nil, fmt.Errorf("failed to build the seccomp filter: %w", err)
	}
	out := make([]byte, 0, 8*len(raw))
	for _, r := range raw {
		out = binary.LittleEndian.AppendUint16(out, r.Op)
		out = append(out, r.Jt, r.Jf)
		out = binary.LittleEndian.AppendUint32(out, r.K)
	}
	return out, nil
}

// lowWordEquals matches when the low 32 bits of argument arg equal v, as
// the kernel reads an int argument: every bit of v set and every other bit
// clear. The library's Equal would compare all 64 bits.
func lowWordEquals(arg uint32, v uint64) seccomp.ArgumentConditions {
	conds := seccomp.ArgumentConditions{{Argument: arg, Operation: seccomp.BitsNotSet, Value: 0xffffffff &^ v}}
	for bit := uint64(1); bit <= v; bit <<= 1 {
		if v&bit != 0 {
			conds = append(conds, seccomp.Condition{Argument: arg, Operation: seccomp.BitsSet, Value: bit})
		}
	}
	return conds
}

// seccompPipe returns the read end of a pipe holding filter, for bwrap to
// read to its end. The filter fits in a pipe's buffer, so the write does not
// wait for a reader.
func seccompPipe(filter []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create the seccomp pipe: %w", err)
	}
	_, err = w.Write(filter)
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("failed to write the seccomp filter: %w", err)
	}
	return r, nil
}
