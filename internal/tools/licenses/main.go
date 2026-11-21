// Command licenses collects the licence and notice files of every module
// that is linked into a built binary.
//
//	go run ./internal/tools/licenses -binary bin/kube-refs-mcp -out bin/licenses
//
// Open source licences ask that their text travels with a binary that
// contains the code. The container image is built from scratch and holds
// nothing but the binary, so the Dockerfile runs this tool and copies the
// result to /licenses.
//
// The list of modules is read from the binary itself, so the set is exactly
// what the binary contains. The tool fails if a
// module has no licence file, rather than leave it out silently.
package main

import (
	"debug/buildinfo"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Files worth shipping: the licence itself, and the notices and patent
// grants some licences require to be passed on.
var (
	licenceFile = regexp.MustCompile(`(?i)^(licen[sc]e|copying|unlicen[sc]e)`)
	extraFile   = regexp.MustCompile(`(?i)^(notice|patents|authors|contributors)`)
)

func main() {
	binary := flag.String("binary", "", "the built binary to read the module list from")
	out := flag.String("out", "", "directory to write the licence files to")
	own := flag.String("project", ".", "directory holding this project's own LICENSE and NOTICE")
	flag.Parse()
	if *binary == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: licenses -binary FILE -out DIR [-project DIR]")
		os.Exit(2)
	}
	if err := run(*binary, *out, *own); err != nil {
		fmt.Fprintln(os.Stderr, "licenses:", err)
		os.Exit(1)
	}
}

func run(binary, out, project string) error {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return fmt.Errorf("reading %s: %w", binary, err)
	}
	cache, err := modCache()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}

	// This project's own licence and notice go at the top.
	if n, err := copyMatching(project, out, true); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("no licence file in %s", project)
	}

	// The Go standard library and runtime are part of every Go binary.
	goroot, err := goEnv("GOROOT")
	if err != nil {
		return err
	}
	// Go's own installer keeps the licence in GOROOT. Linux distributions
	// often move it to their shared licence directory.
	found := false
	for _, dir := range []string{goroot, "/usr/share/licenses/go", "/usr/share/licenses/golang", "/usr/share/doc/golang"} {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if n, err := copyMatching(dir, filepath.Join(out, "go"), true); err != nil {
			return err
		} else if n > 0 {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no licence file for Go itself in %s or the system licence directories", goroot)
	}

	index := []string{"go " + info.GoVersion + " (standard library and runtime)"}
	for _, dep := range info.Deps {
		if dep.Replace != nil {
			dep = dep.Replace
		}
		src := filepath.Join(cache, escape(dep.Path)+"@"+dep.Version)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("module %s %s is not in the module cache (%s); run go mod download", dep.Path, dep.Version, src)
		}
		n, err := copyMatching(src, filepath.Join(out, filepath.FromSlash(dep.Path)), true)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("module %s %s has no licence file", dep.Path, dep.Version)
		}
		index = append(index, dep.Path+" "+dep.Version)
	}
	sort.Strings(index)
	header := "Open source modules built into this program, with their versions.\n" +
		"The licence and notice files of each one are in the directory named after it.\n\n"
	if err := os.WriteFile(filepath.Join(out, "MODULES.txt"), []byte(header+strings.Join(index, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("licences of Go and %d modules written to %s\n", len(index)-1, out)
	return nil
}

// copyMatching copies the licence and notice files at the top of src into
// dst. It returns how many licence files it found.
func copyMatching(src, dst string, extras bool) (int, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	licences := 0
	for _, e := range entries {
		name := e.Name()
		isLicence := licenceFile.MatchString(name)
		if !e.Type().IsRegular() || (!isLicence && !(extras && extraFile.MatchString(name))) {
			continue
		}
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return 0, err
		}
		if isLicence {
			licences++
		}
	}
	return licences, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Files in the module cache are read-only; the copies need not be.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func modCache() (string, error) { return goEnv("GOMODCACHE") }

// goEnv reads a Go environment setting, such as GOMODCACHE or GOROOT.
func goEnv(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	b, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("reading go env %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// escape writes a module path the way the module cache stores it: each
// upper-case letter becomes "!" and its lower-case form.
func escape(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
