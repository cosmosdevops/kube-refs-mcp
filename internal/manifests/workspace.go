package manifests

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// Workspace restricts file access to a set of allowed root directories.
type Workspace struct {
	roots []string
}

// NewWorkspace resolves the given roots. Each root must be an existing directory.
func NewWorkspace(roots []string) (*Workspace, error) {
	w := &Workspace{}
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("workspace root %q: %w", r, err)
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("workspace root %q is not a directory", r)
		}
		w.roots = append(w.roots, real)
	}
	return w, nil
}

// Roots returns the resolved workspace roots.
func (w *Workspace) Roots() []string { return append([]string(nil), w.roots...) }

// Resolve turns a caller-supplied path into a real path inside a workspace
// root. A relative path is tried against each root in order. Symlinks are
// followed before the check, so a link cannot escape the workspace.
func (w *Workspace) Resolve(path string) (string, error) {
	if len(w.roots) == 0 {
		return "", errors.New("file access is disabled: no workspace roots are configured")
	}
	if strings.ContainsRune(path, 0) || path == "" {
		return "", errors.New("invalid path")
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		candidates = candidates[:0]
		for _, root := range w.roots {
			candidates = append(candidates, filepath.Join(root, path))
		}
	}
	for _, candidate := range candidates {
		real, err := filepath.EvalSymlinks(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		if w.within(real) {
			return real, nil
		}
		return "", fmt.Errorf("%s: outside the configured workspace roots", path)
	}
	// Do not reveal whether a path outside the workspace exists. A path that
	// does not exist may still pass through a link that leaves the workspace,
	// so the part of it that does exist is resolved and checked too.
	for _, candidate := range candidates {
		if !w.within(filepath.Clean(candidate)) || !w.within(existingPart(filepath.Clean(candidate))) {
			return "", fmt.Errorf("%s: outside the configured workspace roots", path)
		}
	}
	return "", fmt.Errorf("%s: no such file or directory in the workspace", path)
}

// existingPart resolves the longest leading part of p that exists.
func existingPart(p string) string {
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func (w *Workspace) within(p string) bool {
	for _, root := range w.roots {
		rel, err := filepath.Rel(root, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Display shortens a real path to be relative to its workspace root.
func (w *Workspace) Display(real string) string {
	for _, root := range w.roots {
		if rel, err := filepath.Rel(root, real); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return real
}

// LoadFile reads and parses one manifest file inside the workspace.
func (w *Workspace) LoadFile(path string, lim Limits) ([]Resource, error) {
	real, err := w.Resolve(path)
	if err != nil {
		return nil, err
	}
	return w.loadReal(real, lim)
}

func (w *Workspace) loadReal(real string, lim Limits) ([]Resource, error) {
	data, err := w.readReal(real, lim)
	if err != nil {
		return nil, err
	}
	return Parse(data, w.Display(real), lim)
}

func (w *Workspace) readReal(real string, lim Limits) ([]byte, error) {
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; use the directory check instead", w.Display(real))
	}
	// A pipe or a device never ends, or blocks until something writes to it.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", w.Display(real))
	}
	if lim.MaxBytes > 0 && info.Size() > lim.MaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, larger than the %d byte limit", w.Display(real), info.Size(), lim.MaxBytes)
	}
	return os.ReadFile(real)
}

// withoutPath removes the file's own path from the start of a reason. The
// path is given beside the reason, so it need not be said twice.
// "a.yaml#1: X" becomes "document 1: X", and "a.yaml: X" becomes "X".
func withoutPath(reason, path string) string {
	if rest, ok := strings.CutPrefix(reason, path+"#"); ok {
		return "document " + rest
	}
	if rest, ok := strings.CutPrefix(reason, path+": "); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(reason, path+" "); ok {
		return rest
	}
	return reason
}

// DirLimits bounds a directory walk.
type DirLimits struct {
	Limits
	MaxFiles int
}

// LoadDir parses every .yaml, .yml and .json file under dir.
//
// A document that is not a Kubernetes resource (a Helm values file, a
// kustomization) is left out and listed, and the rest of its file is still
// read. A file that cannot be read at all (not valid YAML, too large, a
// symbolic link) is listed with NotRead set. Its contents are unknown, so a
// report over the directory is then not complete.
func (w *Workspace) LoadDir(dir string, lim DirLimits) ([]Resource, []links.SkippedFile, error) {
	real, err := w.Resolve(dir)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}
	var files []string
	var notRead []links.SkippedFile
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != real && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".yaml", ".yml", ".json":
			if !d.Type().IsRegular() {
				// Never follow a link during a walk: it may leave the
				// workspace. Say so, because the file was not read.
				notRead = append(notRead, links.SkippedFile{Path: w.Display(p), Reason: "not a regular file (a symbolic link, for example), so it was not read", NotRead: true})
				return nil
			}
			files = append(files, p)
			if lim.MaxFiles > 0 && len(files) > lim.MaxFiles {
				return fmt.Errorf("more than %d manifest files under %s", lim.MaxFiles, dir)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var out []Resource
	skipped := notRead
	for _, f := range files {
		shown := w.Display(f)
		data, err := w.readReal(f, lim.Limits)
		if err != nil {
			skipped = append(skipped, links.SkippedFile{Path: shown, Reason: withoutPath(err.Error(), shown), NotRead: true})
			continue
		}
		rs, notResources, err := ParseLenient(data, shown, lim.Limits)
		if err != nil {
			skipped = append(skipped, links.SkippedFile{Path: shown, Reason: withoutPath(err.Error(), shown), NotRead: true})
			continue
		}
		switch {
		case len(notResources) > 0:
			for i := range notResources {
				notResources[i] = withoutPath(notResources[i], shown)
			}
			skipped = append(skipped, links.SkippedFile{Path: shown, Reason: strings.Join(notResources, "; ")})
		case len(rs) == 0:
			skipped = append(skipped, links.SkippedFile{Path: shown, Reason: "no Kubernetes resources found"})
		}
		out = append(out, rs...)
		if lim.MaxResources > 0 && len(out) > lim.MaxResources {
			return nil, nil, fmt.Errorf("more than %d resources under %s", lim.MaxResources, dir)
		}
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
	return out, skipped, nil
}
