//go:build wasip2

package syscall

// Compile-time stubs that let upstream Go's net package build on
// wasip2. wasip2 has no flat-file-descriptor socket API — sockets are
// component-model resources accessed through wasi:sockets. Real socket
// behaviour lives in src/net/*_wasip2.go (which routes through
// internal/poll.Wasip2TCP*) and src/internal/poll/fd_wasip2.go.
//
// Everything below is here purely so the upstream `net` build doesn't
// fail with "undefined: syscall.X" errors. Any of these functions
// reached at runtime returns ENOSYS — the wasip2 net overrides should
// intercept the call paths that matter long before they get here.

// Sockaddr matches upstream Go's net package's expectation of an
// interface-typed address. SockaddrInet4/SockaddrInet6 are already
// declared in syscall_unix.go (//go:build linux || unix) — wasip2
// sets GOOS=linux so we get them from there.
type Sockaddr = any

type SockaddrUnix struct {
	Name string
}

// Address-family / socket-type / protocol constants. AF_INET /
// AF_INET6 are already defined in syscall.go (Linux values, 0x2 / 0xa).
const (
	AF_UNSPEC = 0
	AF_UNIX   = 1
)

const (
	SOCK_STREAM = 1 + iota
	SOCK_DGRAM
	SOCK_RAW
	SOCK_SEQPACKET
)

// SOCK_NONBLOCK / SOCK_CLOEXEC are upstream net's "give me a non-
// blocking + close-on-exec socket" flags. wasip2 sockets are always
// pollable and never inherited, so the bits are essentially no-ops;
// we keep linux-shaped values so any bit-arithmetic upstream net does
// matches what it would on linux.
const (
	SOCK_CLOEXEC  = 0x80000
	SOCK_NONBLOCK = 0x800
)

const (
	IPPROTO_IP   = 0
	IPPROTO_IPV4 = 4
	IPPROTO_IPV6 = 0x29
	IPPROTO_TCP  = 6
	IPPROTO_UDP  = 0x11
)

const SOMAXCONN = 0x80

const (
	IPV6_V6ONLY = 1
	SO_ERROR    = 2
)

const F_DUPFD_CLOEXEC = 1

const RLIMIT_NOFILE = 0

func Getrlimit(which int, lim *Rlimit) error { return ENOSYS }

const (
	SHUT_RD   = 0x1
	SHUT_WR   = 0x2
	SHUT_RDWR = SHUT_RD | SHUT_WR
)

const (
	MSG_PEEK         = 0x1
	MSG_WAITALL      = 0x2
	MSG_CMSG_CLOEXEC = 0x40000000
)

// Socket-option constants used by upstream net's sockopt_*.go files.
// We ship them so the package compiles; actual setsockopt operations
// dispatch through the wasi:sockets interface (or fail with ENOSYS).
const (
	SOL_SOCKET    = 1
	SO_BROADCAST  = 6
	SO_KEEPALIVE  = 9
	SO_LINGER     = 13
	SO_PROTOCOL   = 38
	SO_RCVBUF     = 8
	SO_REUSEADDR  = 2
	SO_SNDBUF     = 7
	SO_TYPE       = 3
	TCP_KEEPCNT   = 6
	TCP_KEEPIDLE  = 4
	TCP_KEEPINTVL = 5
	TCP_NODELAY   = 1

	IP_ADD_MEMBERSHIP   = 35
	IP_MULTICAST_IF     = 32
	IP_MULTICAST_LOOP   = 34
	IPV6_JOIN_GROUP     = 20
	IPV6_LEAVE_GROUP    = 21
	IPV6_MULTICAST_HOPS = 18
	IPV6_MULTICAST_IF   = 17
	IPV6_MULTICAST_LOOP = 19
	IPV6_UNICAST_HOPS   = 16
)

// Linger / IPMreqn exist for upstream net's sockopt code paths. The
// values are inert; nothing on wasip2 reads them.
type Linger struct {
	Onoff  int32
	Linger int32
}

type IPMreqn struct {
	Multiaddr [4]byte
	Interface [4]byte
	Ifindex   int32
}

// Socket-related syscall stubs. None of these are reached on the real
// wasip2 TCP path — net.* dispatches to the wasi:sockets-backed
// helpers in internal/poll/fd_wasip2.go before getting here.

func Socket(proto, sotype, unused int) (int, error) { return -1, ENOSYS }

func Bind(fd int, sa Sockaddr) error { return ENOSYS }

func Listen(fd int, backlog int) error { return ENOSYS }

func Connect(fd int, sa Sockaddr) error { return ENOSYS }

func Accept(fd int) (int, Sockaddr, error) { return -1, nil, ENOSYS }

func Shutdown(fd int, how int) error { return ENOSYS }

func Getsockname(fd int) (Sockaddr, error) { return nil, ENOSYS }

func Getpeername(fd int) (Sockaddr, error) { return nil, ENOSYS }

func Recvfrom(fd int, p []byte, flags int) (int, Sockaddr, error) {
	return 0, nil, ENOSYS
}

func Sendto(fd int, p []byte, flags int, to Sockaddr) error { return ENOSYS }

func Recvmsg(fd int, p, oob []byte, flags int) (n, oobn, recvflags int, from Sockaddr, err error) {
	return 0, 0, 0, nil, ENOSYS
}

func SendmsgN(fd int, p, oob []byte, to Sockaddr, flags int) (int, error) {
	return 0, ENOSYS
}

func GetsockoptInt(fd, level, opt int) (int, error) { return 0, ENOSYS }

func SetsockoptInt(fd, level, opt int, value int) error { return ENOSYS }

func SetNonblock(fd int, nonblocking bool) error { return ENOSYS }

func SetReadDeadline(fd int, t int64) error { return ENOSYS }

func SetWriteDeadline(fd int, t int64) error { return ENOSYS }

func StopIO(fd int) error { return ENOSYS }
