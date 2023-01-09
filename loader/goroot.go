package loader

// This file constructs a new temporary GOROOT directory by merging both the
// standard Go GOROOT and the GOROOT from TinyGo.
//
// The goal is to replace specific packages from Go with a TinyGo version. It's
// never a partial replacement, either a package is fully replaced or it is not.
// This is important because if we did allow to merge packages (e.g. by adding
// files to a package), it would lead to a dependency on implementation details
// with all the maintenance burden that results in. Only allowing to replace
// packages as a whole avoids this as packages are already designed to have a
// public (backwards-compatible) API.

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/goenv"
)

var gorootCreateMutex sync.Mutex

// GetCachedGoroot creates a new GOROOT by merging both the standard GOROOT and
// the GOROOT from TinyGo using lots of symbolic links.
func GetCachedGoroot(config *compileopts.Config) (string, error) {
	goroot := goenv.Get("GOROOT")
	if goroot == "" {
		return "", errors.New("could not determine GOROOT")
	}
	tinygoroot := goenv.Get("TINYGOROOT")
	if tinygoroot == "" {
		return "", errors.New("could not determine TINYGOROOT")
	}

	// Find the overrides needed for the goroot.
	overrides := pathsToOverride(config.GoMinorVersion, needsSyscallPackage(config.BuildTags()))

	// Resolve the merge links within the goroot.
	merge, err := listGorootMergeLinks(goroot, tinygoroot, overrides)
	if err != nil {
		return "", err
	}

	// Hash the merge links to create a cache key.
	data, err := json.Marshal(merge)
	if err != nil {
		return "", err
	}
	hash := sha512.Sum512_256(data)

	// Do not try to create the cached GOROOT in parallel, that's only a waste
	// of I/O bandwidth and thus speed. Instead, use a mutex to make sure only
	// one goroutine does it at a time.
	// This is not a way to ensure atomicity (a different TinyGo invocation
	// could be creating the same directory), but instead a way to avoid
	// creating it many times in parallel when running tests in parallel.
	gorootCreateMutex.Lock()
	defer gorootCreateMutex.Unlock()

	// Check if the goroot already exists.
	cachedGorootName := "goroot-" + hex.EncodeToString(hash[:])
	cachedgoroot := filepath.Join(goenv.Get("GOCACHE"), cachedGorootName)
	if _, err := os.Stat(cachedgoroot); err == nil {
		return cachedgoroot, nil
	}

	// Create the cache directory if it does not already exist.
	err = os.MkdirAll(goenv.Get("GOCACHE"), 0777)
	if err != nil {
		return "", err
	}

	// Create a temporary directory to construct the goroot within.
	tmpgoroot, err := os.MkdirTemp(goenv.Get("GOCACHE"), cachedGorootName+".tmp")
	if err != nil {
		return "", err
	}

	// Remove the temporary directory if it wasn't moved to the right place
	// (for example, when there was an error).
	defer os.RemoveAll(tmpgoroot)

	// Create the directory structure.
	// The directories are created in sorted order so that nested directories are created without extra work.
	{
		var dirs []string
		for dir, merge := range overrides {
			if merge {
				dirs = append(dirs, filepath.Join(tmpgoroot, "src", dir))
			}
		}
		sort.Strings(dirs)

		for _, dir := range dirs {
			err := os.Mkdir(dir, 0777)
			if err != nil {
				return "", err
			}
		}
	}

	for dst, src := range merge {
		err := mirror(src, filepath.Join(tmpgoroot, dst))
		if err != nil {
			return "", err
		}
	}

	// Rename the new merged gorooot into place.
	err = os.Rename(tmpgoroot, cachedgoroot)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another invocation of TinyGo also seems to have created a GOROOT.
			// Use that one instead. Our new GOROOT will be automatically
			// deleted by the defer above.
			return cachedgoroot, nil
		}
		if runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
			// On Windows, a rename with a destination directory that already
			// exists does not result in an IsExist error, but rather in an
			// access denied error. To be sure, check for this case by checking
			// whether the target directory exists.
			if _, err := os.Stat(cachedgoroot); err == nil {
				return cachedgoroot, nil
			}
		}
		return "", err
	}
	return cachedgoroot, nil
}

// listGorootMergeLinks searches goroot and tinygoroot for all files that must
// be created within the merged goroot.
func listGorootMergeLinks(goroot, tinygoroot string, overrides map[string]bool) (map[string]string, error) {
	goSrc := filepath.Join(goroot, "src")
	tinygoSrc := filepath.Join(tinygoroot, "src")
	merges := make(map[string]string)
	for dir, merge := range overrides {
		if !merge {
			// Use the TinyGo version.
			merges[filepath.Join("src", dir)] = filepath.Join(tinygoSrc, dir)
			continue
		}

		// Add files from TinyGo.
		tinygoDir := filepath.Join(tinygoSrc, dir)
		tinygoEntries, err := ioutil.ReadDir(tinygoDir)
		if err != nil {
			return nil, err
		}
		var hasTinyGoFiles bool
		for _, e := range tinygoEntries {
			if e.IsDir() {
				continue
			}

			// Link this file.
			name := e.Name()
			merges[filepath.Join("src", dir, name)] = filepath.Join(tinygoDir, name)

			hasTinyGoFiles = true
		}

		// Add all directories from $GOROOT that are not part of the TinyGo
		// overrides.
		goDir := filepath.Join(goSrc, dir)
		goEntries, err := ioutil.ReadDir(goDir)
		if err != nil {
			return nil, err
		}
		for _, e := range goEntries {
			isDir := e.IsDir()
			if hasTinyGoFiles && !isDir {
				// Only merge files from Go if TinyGo does not have any files.
				// Otherwise we'd end up with a weird mix from both Go
				// implementations.
				continue
			}

			name := e.Name()
			if _, ok := overrides[path.Join(dir, name)+"/"]; ok {
				// This entry is overridden by TinyGo.
				// It has/will be merged elsewhere.
				continue
			}

			// Add a link to this entry
			merges[filepath.Join("src", dir, name)] = filepath.Join(goDir, name)
		}
	}

	// Merge the special directories from goroot.
	for _, dir := range []string{"bin", "lib", "pkg"} {
		merges[dir] = filepath.Join(goroot, dir)
	}

	return merges, nil
}

// needsSyscallPackage returns whether the syscall package should be overriden
// with the TinyGo version. This is the case on some targets.
func needsSyscallPackage(buildTags []string) bool {
	for _, tag := range buildTags {
		if tag == "baremetal" || tag == "darwin" || tag == "nintendoswitch" || tag == "tinygo.wasm" {
			return true
		}
	}
	return false
}

// The boolean indicates whether to merge the subdirs. True means merge, false
// means use the TinyGo version.
func pathsToOverride(goMinor int, needsSyscallPackage bool) map[string]bool {
	paths := map[string]bool{
		"":                      true,
		"crypto/":               true,
		"crypto/rand/":          false,
		"device/":               false,
		"examples/":             false,
		"internal/":             true,
		"internal/fuzz/":        false,
		"internal/bytealg/":     false,
		"internal/reflectlite/": false,
		"internal/task/":        false,
		"machine/":              false,
		"net/":                  true,
		"os/":                   true,
		"reflect/":              false,
		"runtime/":              false,
		"sync/":                 true,
		"testing/":              true,
	}

	if goMinor >= 19 {
		paths["crypto/internal/"] = true
		paths["crypto/internal/boring/"] = true
		paths["crypto/internal/boring/sig/"] = false
	}

	if needsSyscallPackage {
		paths["syscall/"] = true // include syscall/js
	}
	return paths
}

// mirror mirrors the files from oldname to newname.
//
// If oldname is a directory, a directory is created at newname and its content
// is mirrored recursively. Symlinks found in directory trees are not followed.
//
// If oldname is a file, the function first attempts to create a hard link, but
// resorts to making a copy if creating the link fails.
//
// A previous version of this function used symlinks to reference files and
// directories from the Go and Tinygo roots. However, the wasmtime sandbox does
// not follow symlinks, which caused tests to fails (e.g. for os.DirFS).
func mirror(oldname, newname string) error {
	return filepath.Walk(oldname, func(source string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		target := filepath.Join(newname, strings.TrimPrefix(source, oldname))

		if _, err := os.Lstat(target); err == nil {
			return nil // The target already exists, assume its fine?
		}

		if info.IsDir() {
			return os.Mkdir(target, info.Mode())
		}

		if os.Link(source, target) == nil {
			return os.Chmod(target, info.Mode())
		}

		// Making a hardlink failed. Try copying the file as fallback.
		sourceFile, err := os.Open(source)
		if err != nil {
			return err
		}
		defer sourceFile.Close()

		targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer targetFile.Close()

		_, err = io.Copy(targetFile, sourceFile)
		if err != nil {
			os.Remove(target)
		}
		return err
	})
}
