package wasitest

import (
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestTCPEchoWasip1 spawns a TinyGo-compiled wasip1 module that accepts a
// single TCP connection on a host pre-opened socket and echoes the payload
// back. The runtime is wasmtime, invoked with -Stcplisten=host:port.
//
// Like the upstream Go src/internal/runtime/wasitest/tcpecho_test.go, the
// port-probe is racy (the host releases the listener, then asks the
// runtime to re-bind the same port; another process could win in
// between). Test is gated behind TINYGO_WASITEST_TCP=1 so normal CI runs
// don't see flakes.
func TestTCPEchoWasip1(t *testing.T) {
	if os.Getenv("TINYGO_WASITEST_TCP") != "1" {
		t.Skip("set TINYGO_WASITEST_TCP=1 to run wasip1 TCP echo test (racy port probe)")
	}

	tinygo := os.Getenv("TINYGO")
	if tinygo == "" {
		if p, err := exec.LookPath("tinygo"); err == nil {
			tinygo = p
		} else {
			t.Skip("tinygo not found in PATH; set TINYGO to override")
		}
	}
	wasmtime, err := exec.LookPath("wasmtime")
	if err != nil {
		t.Skip("wasmtime not found in PATH")
	}

	tmp := t.TempDir()
	wasm := filepath.Join(tmp, "tcpecho_wasip1.wasm")
	src := filepath.Join("testdata", "tcpecho_wasip1.go")

	build := exec.Command(tinygo, "build", "-target=wasip1", "-o", wasm, src)
	build.Stderr = os.Stderr
	build.Stdout = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("tinygo build: %v", err)
	}

	host, err := probeFreePort()
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}

	run := exec.Command(wasmtime, "run", "-Spreview2=false", "-Stcplisten="+host, wasm)
	var out bytes.Buffer
	run.Stdout = &out
	run.Stderr = &out
	if err := run.Start(); err != nil {
		t.Fatalf("wasmtime start: %v", err)
	}
	defer func() {
		_ = run.Process.Kill()
		_ = run.Wait()
		if t.Failed() {
			t.Logf("wasmtime output:\n%s", out.String())
		}
	}()

	var conn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = net.Dial("tcp", host)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v\nwasmtime output:\n%s", host, err, out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	payload := []byte("foobar")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(conn); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Errorf("payload mismatch: sent %q, got %q", payload, got.Bytes())
	}
}

func probeFreePort() (string, error) {
	port := rand.Intn(10000) + 40000
	for range 20 {
		host := fmt.Sprintf("127.0.0.1:%d", port)
		l, err := net.Listen("tcp", host)
		if err == nil {
			l.Close()
			return host, nil
		}
		port++
	}
	return "", fmt.Errorf("could not find free port")
}
