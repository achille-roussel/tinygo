// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os_test

import (
	"fmt"
	"io"
	"os"
	. "os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// localTmp returns a local temporary directory not on NFS.
func localTmp() string {
	return TempDir()
}

func newFile(testName string, t *testing.T) (f *File) {
	f, err := CreateTemp("", testName)
	if err != nil {
		t.Fatalf("newFile %s: CreateTemp fails with %s", testName, err)
	}
	return
}

func newDir(testName string, t *testing.T) (name string) {
	name, err := os.MkdirTemp(localTmp(), "_Go_"+testName)
	if err != nil {
		t.Fatalf("TempDir %s: %s", testName, err)
	}
	return
}

// Use TempDir (via newFile) to make sure we're on a local file system,
// so that timings are not distorted by latency and caching.
// On NFS, timings can be off due to caching of meta-data on
// NFS servers (Issue 848).
func TestChtimes(t *testing.T) {
	f := newFile("TestChtimes", t)
	defer Remove(f.Name())

	f.Write([]byte("hello, world\n"))
	f.Close()

	testChtimes(t, f.Name())
}

// Use TempDir (via newDir) to make sure we're on a local file system,
// so that timings are not distorted by latency and caching.
// On NFS, timings can be off due to caching of meta-data on
// NFS servers (Issue 848).
func TestChtimesDir(t *testing.T) {
	name := newDir("TestChtimes", t)
	defer RemoveAll(name)

	testChtimes(t, name)
}

func testChtimes(t *testing.T, name string) {
	st, err := Stat(name)
	if err != nil {
		t.Fatalf("Stat %s: %s", name, err)
	}
	preStat := st

	// Move access and modification time back a second
	at := Atime(preStat)
	mt := preStat.ModTime()
	err = Chtimes(name, at.Add(-time.Second), mt.Add(-time.Second))
	if err != nil {
		t.Fatalf("Chtimes %s: %s", name, err)
	}

	st, err = Stat(name)
	if err != nil {
		t.Fatalf("second Stat %s: %s", name, err)
	}
	postStat := st

	pat := Atime(postStat)
	pmt := postStat.ModTime()

	if !pat.Before(at) {
		t.Errorf("Actime didn't go backwards; was=%v, after=%v", mt, pmt)
	}
	if !pmt.Before(mt) {
		t.Errorf("ModTime didn't go backwards; was=%v, after=%v", mt, pmt)
	}
}

// Read with length 0 should not return EOF.
func TestRead0(t *testing.T) {
	f := newFile("TestRead0", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)
	f.Close()
	f, err := Open(f.Name())
	if err != nil {
		t.Errorf("failed to reopen")
	}

	b := make([]byte, 0)
	n, err := f.Read(b)
	if n != 0 || err != nil {
		t.Errorf("Read(0) = %d, %v, want 0, nil", n, err)
	}
	b = make([]byte, 5)
	n, err = f.Read(b)
	if n <= 0 || err != nil {
		t.Errorf("Read(5) = %d, %v, want >0, nil", n, err)
	}
}

// ReadAt with length 0 should not return EOF.
func TestReadAt0(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("TODO: implement Pread for Windows")
		return
	}
	f := newFile("TestReadAt0", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)

	b := make([]byte, 0)
	n, err := f.ReadAt(b, 0)
	if n != 0 || err != nil {
		t.Errorf("ReadAt(0,0) = %d, %v, want 0, nil", n, err)
	}
	b = make([]byte, 5)
	n, err = f.ReadAt(b, 0)
	if n <= 0 || err != nil {
		t.Errorf("ReadAt(5,0) = %d, %v, want >0, nil", n, err)
	}
}

func checkMode(t *testing.T, path string, mode FileMode) {
	dir, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat %q (looking for mode %#o): %s", path, mode, err)
	}
	if dir.Mode()&ModePerm != mode {
		t.Errorf("Stat %q: mode %#o want %#o", path, dir.Mode(), mode)
	}
}

func TestSeek(t *testing.T) {
	if runtime.GOARCH == "386" || runtime.GOARCH == "arm" {
		t.Log("TODO: implement seek for 386 and arm")
		return
	}
	f := newFile("TestSeek", t)
	if f == nil {
		t.Fatalf("f is nil")
		return // TODO: remove
	}
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)

	type test struct {
		in     int64
		whence int
		out    int64
	}
	var tests = []test{
		{0, io.SeekCurrent, int64(len(data))},
		{0, io.SeekStart, 0},
		{5, io.SeekStart, 5},
		{0, io.SeekEnd, int64(len(data))},
		{0, io.SeekStart, 0},
		{-1, io.SeekEnd, int64(len(data)) - 1},
		{1 << 33, io.SeekStart, 1 << 33},
		{1 << 33, io.SeekEnd, 1<<33 + int64(len(data))},

		// Issue 21681, Windows 4G-1, etc:
		{1<<32 - 1, io.SeekStart, 1<<32 - 1},
		{0, io.SeekCurrent, 1<<32 - 1},
		{2<<32 - 1, io.SeekStart, 2<<32 - 1},
		{0, io.SeekCurrent, 2<<32 - 1},
	}
	for i, tt := range tests {
		off, err := f.Seek(tt.in, tt.whence)
		if off != tt.out || err != nil {
			if e, ok := err.(*PathError); ok && e.Err == syscall.EINVAL && tt.out > 1<<32 && runtime.GOOS == "linux" {
				mounts, _ := os.ReadFile("/proc/mounts")
				if strings.Contains(string(mounts), "reiserfs") {
					// Reiserfs rejects the big seeks.
					t.Skipf("skipping test known to fail on reiserfs; https://golang.org/issue/91")
				}
			}
			t.Errorf("#%d: Seek(%v, %v) = %v, %v want %v, nil", i, tt.in, tt.whence, off, err, tt.out)
		}
	}
}

func TestReadAt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("TODO: implement Pread for Windows")
		return
	}
	f := newFile("TestReadAt", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)

	b := make([]byte, 5)
	n, err := f.ReadAt(b, 7)
	if err != nil || n != len(b) {
		t.Fatalf("ReadAt 7: %d, %v", n, err)
	}
	if string(b) != "world" {
		t.Fatalf("ReadAt 7: have %q want %q", string(b), "world")
	}
}

// Verify that ReadAt doesn't affect seek offset.
// In the Plan 9 kernel, there used to be a bug in the implementation of
// the pread syscall, where the channel offset was erroneously updated after
// calling pread on a file.
func TestReadAtOffset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("TODO: implement Pread for Windows")
		return
	}
	f := newFile("TestReadAtOffset", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)
	f.Close()
	f, err := Open(f.Name())
	if err != nil {
		t.Errorf("failed to reopen")
	}

	b := make([]byte, 5)

	n, err := f.ReadAt(b, 7)
	if err != nil || n != len(b) {
		t.Fatalf("ReadAt 7: %d, %v", n, err)
	}
	if string(b) != "world" {
		t.Fatalf("ReadAt 7: have %q want %q", string(b), "world")
	}

	n, err = f.Read(b)
	if err != nil || n != len(b) {
		t.Fatalf("Read: %d, %v", n, err)
	}
	if string(b) != "hello" {
		t.Fatalf("Read: have %q want %q", string(b), "hello")
	}
}

// Verify that ReadAt doesn't allow negative offset.
func TestReadAtNegativeOffset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("TODO: implement Pread for Windows")
		return
	}
	f := newFile("TestReadAtNegativeOffset", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)
	f.Close()
	f, err := Open(f.Name())
	if err != nil {
		t.Errorf("failed to reopen")
	}

	b := make([]byte, 5)

	n, err := f.ReadAt(b, -10)

	const wantsub = "negative offset"
	if !strings.Contains(err.Error(), wantsub) || n != 0 {
		t.Errorf("ReadAt(-10) = %v, %v; want 0, ...%q...", n, err, wantsub)
	}
}

func TestReadAtEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("TODO: implement Pread for Windows")
		return
	}
	f := newFile("TestReadAtEOF", t)
	defer Remove(f.Name())
	defer f.Close()

	_, err := f.ReadAt(make([]byte, 10), 0)
	switch err {
	case io.EOF:
		// all good
	case nil:
		t.Fatalf("ReadAt succeeded")
	default:
		t.Fatalf("ReadAt failed: %s", err)
	}
}

func TestWriteAt(t *testing.T) {
	f := newFile("TestWriteAt", t)
	defer Remove(f.Name())
	defer f.Close()

	const data = "hello, world\n"
	io.WriteString(f, data)

	n, err := f.WriteAt([]byte("WORLD"), 7)
	if err != nil || n != 5 {
		t.Fatalf("WriteAt 7: %d, %v", n, err)
	}

	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile %s: %v", f.Name(), err)
	}
	if string(b) != "hello, WORLD\n" {
		t.Fatalf("after write: have %q want %q", string(b), "hello, WORLD\n")
	}
}

// Verify that WriteAt doesn't allow negative offset.
func TestWriteAtNegativeOffset(t *testing.T) {
	f := newFile("TestWriteAtNegativeOffset", t)
	defer Remove(f.Name())
	defer f.Close()

	n, err := f.WriteAt([]byte("WORLD"), -10)

	const wantsub = "negative offset"
	if !strings.Contains(fmt.Sprint(err), wantsub) || n != 0 {
		t.Errorf("WriteAt(-10) = %v, %v; want 0, ...%q...", n, err, wantsub)
	}
}

// Verify that WriteAt doesn't work in append mode.
func TestWriteAtInAppendMode(t *testing.T) {
	defer chtmpdir(t)()
	f, err := OpenFile("write_at_in_append_mode.txt", O_APPEND|O_CREATE, 0666)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	_, err = f.WriteAt([]byte(""), 1)
	if err != ErrWriteAtInAppendMode {
		t.Fatalf("f.WriteAt returned %v, expected %v", err, ErrWriteAtInAppendMode)
	}
}
