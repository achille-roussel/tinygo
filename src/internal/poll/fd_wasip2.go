//go:build wasip2

// fd_wasip2.go is the wasip2 sibling of fd_wasip1.go: it backs net.TCPListener
// and net.TCPConn with the runtime's pollable-keyed netpoll registry (see
// runtime/netpoll_wasip2.go). The Sysfd of fd_wasip1.go is replaced with a
// triple of wasi resource handles: the tcp-socket itself plus, for an
// accepted/connected connection, the (input-stream, output-stream) pair.

package poll

import (
	"errors"
	"internal/cm"
	"internal/task"
	"internal/wasi/io/v0.2.0/poll"
	wasistreams "internal/wasi/io/v0.2.0/streams"
	"internal/wasi/sockets/v0.2.0/instance-network"
	wasinet "internal/wasi/sockets/v0.2.0/network"
	wasitcp "internal/wasi/sockets/v0.2.0/tcp"
	wasitcpcreate "internal/wasi/sockets/v0.2.0/tcp-create-socket"
	"time"
	"unsafe"
)

// ErrFileClosingWasip2 distinguishes the wasip2 close error; reusing the
// wasip1 ErrFileClosing keeps callers in upstream net happy.

//go:linkname runtime_netpoll_addpollable_wasip2 runtime.runtime_netpoll_addpollable_wasip2
func runtime_netpoll_addpollable_wasip2(pollable uint32) uintptr

//go:linkname runtime_netpoll_done_wasip2 runtime.runtime_netpoll_done_wasip2
func runtime_netpoll_done_wasip2(pd uintptr)

//go:linkname runtime_netpoll_pdfired_wasip2 runtime.runtime_netpoll_pdfired_wasip2
func runtime_netpoll_pdfired_wasip2(pd uintptr) bool

//go:linkname runtime_netpoll_wake_wasip2 runtime.runtime_netpoll_wake_wasip2
func runtime_netpoll_wake_wasip2(pd uintptr)

// network is the lazy-initialised wasi:sockets/instance-network handle.
// All TCP operations need a network; instance-network() returns the host's
// default network. We hold a single handle for the program's lifetime.
var (
	wasip2Network     wasinet.Network
	wasip2NetworkInit bool
)

func wasip2GetNetwork() wasinet.Network {
	if !wasip2NetworkInit {
		wasip2Network = instancenetwork.InstanceNetwork()
		wasip2NetworkInit = true
	}
	return wasip2Network
}

// WasipNFD (named to avoid colliding with the wasip1 FD type that ships in
// the same package via fd_wasip1.go) is the wasip2 file descriptor wrapper.
// Each FD is either a listener (input/output zero-valued) or a connection
// (all three valid).
//
// Public field naming mirrors fd_wasip1.go where it makes sense; callers in
// src/net/*_wasip2.go construct it via the open / dial / listen helpers
// below rather than struct literal.
type WasipNFD struct {
	// Sysfd is a sentinel surfaced as int so that upstream Go's net
	// package (which expects poll.FD to have an integer fd) compiles.
	// Its value is the address of the WasipNFD itself reinterpreted —
	// there is no real OS file descriptor on wasip2. Callers must not
	// pass this through to syscalls.
	Sysfd int

	socket     wasitcp.TCPSocket
	input      wasistreams.InputStream
	output     wasistreams.OutputStream
	isListener bool
	closed     bool

	// IsStream / ZeroReadIsEOF mirror the fd_wasip1 fields so net's
	// fd_posix.go compiles. We don't read them — TCP is always stream
	// and our Read returns io.EOF on its own.
	IsStream      bool
	ZeroReadIsEOF bool

	rDeadline time.Time
	wDeadline time.Time
}

// FD is the type name upstream Go's net package expects in internal/poll.
// On wasip2 it aliases WasipNFD — same struct, different name to match
// the fd_wasip1.go convention (where the wasip1 FD type is used as
// poll.FD by upstream net's fd_posix.go).
type FD = WasipNFD

// errorCodeToError maps a wasi network ErrorCode into a Go error.
func errorCodeToError(c wasinet.ErrorCode) error {
	if c == wasinet.ErrorCodeWouldBlock {
		return errWasip2WouldBlock
	}
	return errors.New("wasip2 network: " + c.String())
}

var errWasip2WouldBlock = errors.New("would block")

// DialTCPWasip2 creates a TCP socket, starts a connect to remote, parks
// until finish-connect succeeds, and returns the resulting connection FD.
func DialTCPWasip2(remoteIPv4 [4]byte, remotePort uint16) (*WasipNFD, error) {
	sockRes := wasitcpcreate.CreateTCPSocket(wasinet.IPAddressFamilyIPv4)
	if sockRes.IsErr() {
		return nil, errorCodeToError(*sockRes.Err())
	}
	sock := *sockRes.OK()

	addr := wasinet.IPSocketAddressIPv4(wasinet.IPv4SocketAddress{
		Port:    remotePort,
		Address: remoteIPv4,
	})

	startRes := sock.StartConnect(wasip2GetNetwork(), addr)
	if startRes.IsErr() {
		sock.ResourceDrop()
		return nil, errorCodeToError(*startRes.Err())
	}

	for {
		finRes := sock.FinishConnect()
		if !finRes.IsErr() {
			tup := finRes.OK()
			return &WasipNFD{
				socket: sock,
				input:  tup.F0,
				output: tup.F1,
			}, nil
		}
		ec := *finRes.Err()
		if ec != wasinet.ErrorCodeWouldBlock {
			sock.ResourceDrop()
			return nil, errorCodeToError(ec)
		}
		// park on the socket's pollable until connect completes
		waitOnPollable(sock.Subscribe())
	}
}

// ListenTCPWasip2 creates a TCP socket, binds it to localIPv4:localPort,
// puts it in listening mode, and returns the listener FD.
func ListenTCPWasip2(localIPv4 [4]byte, localPort uint16) (*WasipNFD, error) {
	sockRes := wasitcpcreate.CreateTCPSocket(wasinet.IPAddressFamilyIPv4)
	if sockRes.IsErr() {
		return nil, errorCodeToError(*sockRes.Err())
	}
	sock := *sockRes.OK()

	addr := wasinet.IPSocketAddressIPv4(wasinet.IPv4SocketAddress{
		Port:    localPort,
		Address: localIPv4,
	})

	if r := sock.StartBind(wasip2GetNetwork(), addr); r.IsErr() {
		sock.ResourceDrop()
		return nil, errorCodeToError(*r.Err())
	}
	for {
		r := sock.FinishBind()
		if !r.IsErr() {
			break
		}
		ec := *r.Err()
		if ec != wasinet.ErrorCodeWouldBlock {
			sock.ResourceDrop()
			return nil, errorCodeToError(ec)
		}
		waitOnPollable(sock.Subscribe())
	}

	if r := sock.StartListen(); r.IsErr() {
		sock.ResourceDrop()
		return nil, errorCodeToError(*r.Err())
	}
	for {
		r := sock.FinishListen()
		if !r.IsErr() {
			break
		}
		ec := *r.Err()
		if ec != wasinet.ErrorCodeWouldBlock {
			sock.ResourceDrop()
			return nil, errorCodeToError(ec)
		}
		waitOnPollable(sock.Subscribe())
	}

	return &WasipNFD{socket: sock, isListener: true}, nil
}

// LocalAddr returns the bound local address of this FD (listener or
// connection). Returns nil if the wasi runtime doesn't surface it.
func (fd *WasipNFD) LocalAddr() (ip [4]byte, port uint16, ok bool) {
	r := fd.socket.LocalAddress()
	if r.IsErr() {
		return ip, 0, false
	}
	addr := r.OK()
	if v4 := addr.IPv4(); v4 != nil {
		return v4.Address, v4.Port, true
	}
	return ip, 0, false
}

// Accept blocks until an incoming connection is available, then returns
// the connection FD. Honours fd.rDeadline.
func (fd *WasipNFD) Accept() (*WasipNFD, error) {
	if fd.closed {
		return nil, ErrFileClosing
	}
	deadline := fd.rDeadline
	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, ErrDeadlineExceeded
		}
		r := fd.socket.Accept()
		if !r.IsErr() {
			tup := r.OK()
			return &WasipNFD{
				socket: tup.F0,
				input:  tup.F1,
				output: tup.F2,
			}, nil
		}
		ec := *r.Err()
		if ec != wasinet.ErrorCodeWouldBlock {
			return nil, errorCodeToError(ec)
		}
		if deadline.IsZero() {
			waitOnPollable(fd.socket.Subscribe())
		} else {
			if err := waitOnPollableUntil(fd.socket.Subscribe(), deadline); err != nil {
				return nil, err
			}
		}
	}
}

// Read reads from the connection's input stream. Returns (0, nil) on EOF.
func (fd *WasipNFD) Read(p []byte) (int, error) {
	if fd.closed {
		return 0, ErrFileClosing
	}
	if len(p) == 0 {
		return 0, nil
	}
	if fd.isListener {
		return 0, errors.New("read on listener FD")
	}
	deadline := fd.rDeadline
	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, ErrDeadlineExceeded
		}
		r := fd.input.Read(uint64(len(p)))
		if r.IsErr() {
			se := r.Err()
			if se.Closed() {
				return 0, nil // EOF
			}
			return 0, errors.New("wasip2 stream read failed")
		}
		data := r.OK().Slice()
		if len(data) > 0 {
			n := copy(p, data)
			return n, nil
		}
		// No data available — park on the input stream's pollable.
		if deadline.IsZero() {
			waitOnPollable(fd.input.Subscribe())
		} else {
			if err := waitOnPollableUntil(fd.input.Subscribe(), deadline); err != nil {
				return 0, err
			}
		}
	}
}

// Write writes p to the connection's output stream. Loops until all of p
// is written or an error occurs. Honours fd.wDeadline.
func (fd *WasipNFD) Write(p []byte) (int, error) {
	if fd.closed {
		return 0, ErrFileClosing
	}
	if fd.isListener {
		return 0, errors.New("write on listener FD")
	}
	deadline := fd.wDeadline
	var nn int
	for nn < len(p) {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nn, ErrDeadlineExceeded
		}
		cw := fd.output.CheckWrite()
		if cw.IsErr() {
			se := cw.Err()
			if se.Closed() {
				return nn, errors.New("wasip2 stream closed")
			}
			return nn, errors.New("wasip2 stream write check failed")
		}
		canWrite := uint64(*cw.OK())
		if canWrite == 0 {
			if deadline.IsZero() {
				waitOnPollable(fd.output.Subscribe())
			} else {
				if err := waitOnPollableUntil(fd.output.Subscribe(), deadline); err != nil {
					return nn, err
				}
			}
			continue
		}
		chunk := uint64(len(p) - nn)
		if chunk > canWrite {
			chunk = canWrite
		}
		wr := fd.output.Write(cm.ToList(p[nn : nn+int(chunk)]))
		if wr.IsErr() {
			se := wr.Err()
			if se.Closed() {
				return nn, errors.New("wasip2 stream closed")
			}
			return nn, errors.New("wasip2 stream write failed")
		}
		nn += int(chunk)
	}
	return nn, nil
}

// Close drops all wasi resources held by the FD. Idempotent in the sense
// that a second call returns ErrFileClosing without re-dropping (resource
// drop is once-only in the component model).
func (fd *WasipNFD) Close() error {
	if fd.closed {
		return ErrFileClosing
	}
	fd.closed = true
	// Drop streams first (they reference the socket).
	var zeroIn wasistreams.InputStream
	if fd.input != zeroIn {
		fd.input.ResourceDrop()
	}
	var zeroOut wasistreams.OutputStream
	if fd.output != zeroOut {
		fd.output.ResourceDrop()
	}
	fd.socket.ResourceDrop()
	return nil
}

func (fd *WasipNFD) SetDeadline(t time.Time) error {
	fd.rDeadline = t
	fd.wDeadline = t
	return nil
}

func (fd *WasipNFD) SetReadDeadline(t time.Time) error {
	fd.rDeadline = t
	return nil
}

func (fd *WasipNFD) SetWriteDeadline(t time.Time) error {
	fd.wDeadline = t
	return nil
}

// waitOnPollable transfers ownership of the pollable to the runtime
// registry, parks the current goroutine, and returns when the runtime
// drops the pollable + wakes us.
func waitOnPollable(p poll.Pollable) {
	handle := cm.Reinterpret[uint32](p)
	pd := runtime_netpoll_addpollable_wasip2(handle)
	task.Pause()
	runtime_netpoll_done_wasip2(pd)
}

// waitOnPollableUntil parks on the pollable but arms a time.AfterFunc that
// wakes the task if the deadline expires first. Mirrors the wasip1 parkUntil
// pattern from fd_wasip1.go.
func waitOnPollableUntil(p poll.Pollable, deadline time.Time) error {
	d := time.Until(deadline)
	if d <= 0 {
		// Don't even register: drop the pollable and report timeout.
		p.ResourceDrop()
		return ErrDeadlineExceeded
	}
	handle := cm.Reinterpret[uint32](p)
	pd := runtime_netpoll_addpollable_wasip2(handle)
	timer := time.AfterFunc(d, func() {
		runtime_netpoll_wake_wasip2(pd)
	})
	task.Pause()
	timer.Stop()
	runtime_netpoll_done_wasip2(pd)
	return nil
}

// Linkname-friendly wrappers around the WasipNFD methods. They use
// uintptr for the FD pointer so callers can hold the FD via a raw
// handle without needing the WasipNFD type in scope (the type itself
// can't easily be linknamed). Used by tests / future net package code.
//
//go:linkname Wasip2TCPListen
func Wasip2TCPListen(ipv4 [4]byte, port uint16) (uintptr, error) {
	fd, err := ListenTCPWasip2(ipv4, port)
	if err != nil {
		return 0, err
	}
	return uintptr(unsafe.Pointer(fd)), nil
}

//go:linkname Wasip2TCPDial
func Wasip2TCPDial(ipv4 [4]byte, port uint16) (uintptr, error) {
	fd, err := DialTCPWasip2(ipv4, port)
	if err != nil {
		return 0, err
	}
	return uintptr(unsafe.Pointer(fd)), nil
}

//go:linkname Wasip2TCPAccept
func Wasip2TCPAccept(listener uintptr) (uintptr, error) {
	fd := (*WasipNFD)(unsafe.Pointer(listener))
	accepted, err := fd.Accept()
	if err != nil {
		return 0, err
	}
	return uintptr(unsafe.Pointer(accepted)), nil
}

//go:linkname Wasip2TCPRead
func Wasip2TCPRead(conn uintptr, p []byte) (int, error) {
	fd := (*WasipNFD)(unsafe.Pointer(conn))
	return fd.Read(p)
}

//go:linkname Wasip2TCPWrite
func Wasip2TCPWrite(conn uintptr, p []byte) (int, error) {
	fd := (*WasipNFD)(unsafe.Pointer(conn))
	return fd.Write(p)
}

//go:linkname Wasip2TCPClose
func Wasip2TCPClose(fd uintptr) error {
	return (*WasipNFD)(unsafe.Pointer(fd)).Close()
}

//go:linkname Wasip2TCPSetDeadline
func Wasip2TCPSetDeadline(fd uintptr, t time.Time) error {
	return (*WasipNFD)(unsafe.Pointer(fd)).SetDeadline(t)
}

// The following methods and package-level helpers exist so that
// upstream Go's net package compiles unchanged on wasip2 — fd_posix.go,
// fd_unix.go, sock_posix.go, sockopt_*.go and friends call them on the
// netFD's underlying poll.FD. Most of them are no-ops or return errors
// on wasip2 because the real wasip2 socket lifecycle is driven through
// the src/net/*_wasip2.go overrides (not yet written), which talk to
// wasi:sockets directly rather than going through this shim. They're
// kept here so a partial build still typechecks while the overrides
// are being written.

// Init brings the FD up to a usable state. On wasip2 there's nothing
// to do — wasi:sockets resources are created already-pollable.
func (fd *WasipNFD) Init(net string, pollable bool) error {
	fd.IsStream = net == "tcp" || net == "tcp4" || net == "tcp6" || net == "unix"
	fd.ZeroReadIsEOF = fd.IsStream
	return nil
}

// Shutdown is reserved for the wasip2 TCP path. wasi:sockets/tcp
// exposes shutdown directly on the TcpSocket resource; the src/net
// override calls it without going through this method.
func (fd *WasipNFD) Shutdown(how int) error {
	if fd.closed {
		return ErrFileClosing
	}
	return errors.New("wasip2 Shutdown: not yet implemented")
}

// CloseRead / CloseWrite drop one half of the duplex socket.
func (fd *WasipNFD) CloseRead() error  { return fd.Shutdown(0) }
func (fd *WasipNFD) CloseWrite() error { return fd.Shutdown(1) }

// RawControl invokes f with the FD's Sysfd. wasip2 doesn't have a real
// fd, but upstream net uses RawControl only via syscall.RawConn — the
// callback is wasip2-aware code we control.
func (fd *WasipNFD) RawControl(f func(uintptr)) error {
	if fd.closed {
		return ErrFileClosing
	}
	f(uintptr(fd.Sysfd))
	return nil
}

func (fd *WasipNFD) RawRead(f func(uintptr) bool) error {
	if fd.closed {
		return ErrFileClosing
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
		// wasip2 RawRead can't park on a syscall fd — best-effort retry.
	}
}

func (fd *WasipNFD) RawWrite(f func(uintptr) bool) error {
	if fd.closed {
		return ErrFileClosing
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
	}
}

// WaitWrite parks until the FD is writable. On wasip2 we lean on
// the underlying output stream's pollable.
func (fd *WasipNFD) WaitWrite() error {
	if fd.closed {
		return ErrFileClosing
	}
	if fd.isListener {
		return errors.New("WaitWrite on listener FD")
	}
	cw := fd.output.CheckWrite()
	if cw.IsErr() {
		return errors.New("wasip2 WaitWrite: stream closed")
	}
	if *cw.OK() > 0 {
		return nil
	}
	waitOnPollable(fd.output.Subscribe())
	return nil
}

// Dup duplicates the FD. Not supported on wasip2 — resources don't
// have a dup primitive in wasi:sockets.
func (fd *WasipNFD) Dup() (int, string, error) {
	return -1, "dup", errors.New("wasip2: Dup not supported")
}

// SetsockoptInt / GetsockoptInt / SetsockoptByte / etc. Stubs.
func (fd *WasipNFD) SetsockoptInt(level, opt, value int) error  { return nil }
func (fd *WasipNFD) GetsockoptInt(level, opt int) (int, error)  { return 0, nil }
func (fd *WasipNFD) SetsockoptByte(level, opt int, value byte) error { return nil }
func (fd *WasipNFD) SetsockoptLinger(level, opt int, l *Linger) error { return nil }
func (fd *WasipNFD) SetsockoptInet4Addr(level, opt int, value [4]byte) error { return nil }
func (fd *WasipNFD) SetsockoptIPMreq(level, opt int, mreq *IPMreq) error     { return nil }
func (fd *WasipNFD) SetsockoptIPMreqn(level, opt int, mreq *IPMreqn) error   { return nil }
func (fd *WasipNFD) SetsockoptIPv6Mreq(level, opt int, mreq *IPv6Mreq) error { return nil }

// ReadLock / ReadUnlock / WriteLock / WriteUnlock are mutex hooks on
// upstream's poll.FD. No-op on wasip2 (our Read/Write paths are
// goroutine-safe via the cooperative scheduler).
func (fd *WasipNFD) ReadLock() error    { return nil }
func (fd *WasipNFD) ReadUnlock()        {}
func (fd *WasipNFD) WriteLock() error   { return nil }
func (fd *WasipNFD) WriteUnlock()       {}

// WriteOnce is upstream's single-syscall write entry point used by
// connect retries. We just forward to Write.
func (fd *WasipNFD) WriteOnce(p []byte) (int, error) { return fd.Write(p) }

// String is the upstream poll-package alias for a non-importable
// string type. Defined here so files referencing poll.String compile.
type String string

// IPMreq / IPv6Mreq are the legacy multicast structs upstream's
// sockopt code references. We don't support multicast on wasip2;
// these exist solely so the types resolve at compile time.
type IPMreq struct {
	Multiaddr [4]byte
	Interface [4]byte
}

type IPv6Mreq struct {
	Multiaddr [16]byte
	Interface uint32
}

// Linger is the SO_LINGER struct upstream sockopt_posix.go uses.
type Linger struct {
	Onoff  int32
	Linger int32
}

// IPMreqn is the linux multicast struct.
type IPMreqn struct {
	Multiaddr [4]byte
	Interface [4]byte
	Ifindex   int32
}

// CloseFunc is the hook upstream uses to override Close in tests. We
// don't have a unix fd to close, so default to a no-op.
var CloseFunc func(int) error = func(int) error { return nil }

// AcceptFunc mirrors CloseFunc — overridable by tests. No-op default.
var AcceptFunc func(int) (int, any, error) = func(int) (int, any, error) {
	return -1, nil, errors.New("wasip2: AcceptFunc not implemented")
}

// DupCloseOnExec is the upstream helper used by os.File to duplicate
// an fd with FD_CLOEXEC set. Not supported on wasip2.
func DupCloseOnExec(fd int) (int, string, error) {
	return -1, "dup", errors.New("wasip2: DupCloseOnExec not supported")
}

// SendFile / Splice are referenced by upstream sendfile_linux.go and
// splice_linux.go. We provide stubs returning (0, ENOSYS, false) so
// the upstream files compile; the actual code path is shadowed
// elsewhere with TinyGo no-op files.
func SendFile(dstFD *FD, src int, size int64) (int64, error, bool) {
	return 0, errors.New("wasip2: SendFile not supported"), false
}

func Splice(dst, src *FD, max int64) (int64, int64, error) {
	return 0, 0, errors.New("wasip2: Splice not supported")
}
