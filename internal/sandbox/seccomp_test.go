package sandbox

import (
	"encoding/binary"
	"io"
	"testing"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/bpf"
)

// call is one seccomp_data for the VM, which loads words big-endian: each
// 32-bit field is written so, with an argument's low word first as on the
// little-endian hosts the filter is assembled for.
func call(nr int, auditArch uint32, args ...uint64) []byte {
	data := make([]byte, 64)
	binary.BigEndian.PutUint32(data[0:], uint32(nr))
	binary.BigEndian.PutUint32(data[4:], auditArch)
	for i, a := range args {
		binary.BigEndian.PutUint32(data[16+8*i:], uint32(a))
		binary.BigEndian.PutUint32(data[20+8*i:], uint32(a>>32))
	}
	return data
}

func TestSeccompFilter(t *testing.T) {
	info, err := arch.GetInfo("")
	if err != nil {
		t.Skip(err)
	}
	filter, err := seccompFilter()
	require.NoError(t, err)
	require.Zero(t, len(filter)%8)
	var prog []bpf.Instruction
	for i := 0; i < len(filter); i += 8 {
		prog = append(prog, bpf.RawInstruction{
			Op: binary.LittleEndian.Uint16(filter[i:]),
			Jt: filter[i+2],
			Jf: filter[i+3],
			K:  binary.LittleEndian.Uint32(filter[i+4:]),
		}.Disassemble())
	}
	vm, err := bpf.NewVM(prog)
	require.NoError(t, err)
	nr := func(name string) int {
		n, ok := info.SyscallNames[name]
		require.True(t, ok, name)
		return n
	}
	const afInet = 2
	eperm := uint32(seccomp.ActionErrno) | 1
	allow := uint32(seccomp.ActionAllow)

	for _, tc := range []struct {
		name string
		data []byte
		want uint32
	}{
		{"unix socket", call(nr("socket"), uint32(info.ID), afUnix, 1), eperm},
		{"unix socket with high bits", call(nr("socket"), uint32(info.ID), 1<<32|afUnix), eperm},
		{"unix datagram socketpair", call(nr("socketpair"), uint32(info.ID), afUnix, 2), eperm},
		{"unix datagram socketpair with flags", call(nr("socketpair"), uint32(info.ID), afUnix, 2|0o4000|0o2000000), eperm},
		{"unix raw socketpair", call(nr("socketpair"), uint32(info.ID), afUnix, 3), eperm},
		{"unix raw socketpair with flags", call(nr("socketpair"), uint32(info.ID), afUnix, 3|0o4000|0o2000000), eperm},
		{"unix other socketpair type with bit 1", call(nr("socketpair"), uint32(info.ID), afUnix, 6), allow},
		{"unix stream socketpair", call(nr("socketpair"), uint32(info.ID), afUnix, 1), allow},
		{"unix stream socketpair with flags", call(nr("socketpair"), uint32(info.ID), afUnix, 1|0o4000|0o2000000), allow},
		{"unix seqpacket socketpair", call(nr("socketpair"), uint32(info.ID), afUnix, 5), allow},
		{"unix socketpair with high bits", call(nr("socketpair"), uint32(info.ID), 1<<40|afUnix, 2), eperm},
		{"inet socketpair", call(nr("socketpair"), uint32(info.ID), afInet, 2), allow},
		{"other socketpair family with bit 0", call(nr("socketpair"), uint32(info.ID), 3, 2), allow},
		{"inet socket", call(nr("socket"), uint32(info.ID), afInet, 1), allow},
		{"other family with bit 0", call(nr("socket"), uint32(info.ID), 3), allow},
		{"io_uring_setup", call(nr("io_uring_setup"), uint32(info.ID)), eperm},
		{"io_uring_enter", call(nr("io_uring_enter"), uint32(info.ID)), eperm},
		{"io_uring_register", call(nr("io_uring_register"), uint32(info.ID)), eperm},
		{"connect", call(nr("connect"), uint32(info.ID), 3), allow},
		{"read", call(nr("read"), uint32(info.ID)), allow},
		{"vsock", call(nr("socket"), uint32(info.ID), afVsock, 1), eperm},
		{"vsock with high bits", call(nr("socket"), uint32(info.ID), 1<<40|afVsock), eperm},
		{"one bit of vsock", call(nr("socket"), uint32(info.ID), 8), allow},
		{"more bits than vsock", call(nr("socket"), uint32(info.ID), afVsock|1), allow},
		{"unshare user", call(nr("unshare"), uint32(info.ID), cloneNewUser|0x20000), eperm},
		{"unshare user with high bits", call(nr("unshare"), uint32(info.ID), 1<<32|cloneNewUser), eperm},
		{"unshare mount", call(nr("unshare"), uint32(info.ID), 0x20000), allow},
		{"clone user", call(nr("clone"), uint32(info.ID), cloneNewUser|17), eperm},
		{"clone thread", call(nr("clone"), uint32(info.ID), 0x3d0f00), allow},
		{"clone with high bits", call(nr("clone"), uint32(info.ID), 1<<33|17), allow},
		{"clone3", call(nr("clone3"), uint32(info.ID)), uint32(seccomp.ActionErrno) | enosys},
		{"other ABI", call(nr("socket"), uint32(info.ID)+1, afUnix), uint32(seccomp.ActionKillProcess)},
	} {
		got, err := vm.Run(tc.data)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, uint32(got), tc.name)
	}
}

func TestSeccompPipe(t *testing.T) {
	r, err := seccompPipe([]byte("filter"))
	require.NoError(t, err)
	defer r.Close()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "filter", string(got))
}
