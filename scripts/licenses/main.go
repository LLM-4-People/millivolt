// Command licenses bundles the notices from the exact target binary dependency
// graph. It does not classify licenses or replace a distribution license review.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/LLM-4-People/millivolt"
)

type module struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *module
}
type pkg struct {
	Dir        string
	ImportPath string
	Standard   bool
	Module     *module
	EmbedFiles []string
}
type notice struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type entry struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	Files   []notice `json:"files"`
}
type source struct {
	root  string
	dirs  map[string]bool
	extra map[string]string
}

func main() {
	target := flag.String("target", runtime.GOOS+"/"+runtime.GOARCH, "target GOOS/GOARCH")
	output := flag.String("out", "", "new output directory")
	flag.Parse()
	if flag.NArg() != 0 || *output == "" {
		fatal(fmt.Errorf("usage: licenses -out NEW_DIRECTORY [-target GOOS/GOARCH]"))
	}
	packages, err := dependencies(*target)
	if err == nil {
		err = bundle(*output, packages)
	}
	if err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, "licenses:", err); os.Exit(1) }
func dependencies(target string) ([]pkg, error) {
	parts := strings.Split(target, "/")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return nil, fmt.Errorf("unsupported release target %q", target)
	}
	cmd := exec.Command("go", "list", "-mod=readonly", "-deps", "-json=Dir,ImportPath,Standard,Module,EmbedFiles", "./cmd/proxy")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var result []pkg
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty dependency graph")
	}
	return result, nil
}
func licenseName(name string) bool {
	if strings.EqualFold(filepath.Ext(name), ".go") {
		return false
	}
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "COPYRIGHT", "NOTICE", "PATENTS"} {
		if upper == prefix || strings.HasPrefix(upper, prefix+".") || strings.HasPrefix(upper, prefix+"-") ||
			strings.HasSuffix(upper, "-"+prefix) || strings.HasSuffix(upper, "."+prefix) {
			return true
		}
	}
	return false
}
func addAncestors(s *source, dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("dependency directory outside its module")
	}
	for {
		s.dirs[dir] = true
		if dir == s.root {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}
func collect(s *source) (map[string]string, error) {
	files := map[string]string{}
	for dir := range s.dirs {
		items, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			path := filepath.Join(dir, item.Name())
			if item.Type()&os.ModeSymlink != 0 {
				if licenseName(item.Name()) {
					return nil, fmt.Errorf("symlinked notice requires distribution review: %s", item.Name())
				}
				continue
			}
			if !item.IsDir() && licenseName(item.Name()) {
				rel, _ := filepath.Rel(s.root, path)
				files[filepath.ToSlash(rel)] = path
			} else if item.IsDir() && (strings.EqualFold(item.Name(), "licenses") || strings.EqualFold(item.Name(), "licences")) {
				err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.Type()&os.ModeSymlink != 0 {
						return fmt.Errorf("symlink in license directory")
					}
					if d.IsDir() {
						return nil
					}
					rel, _ := filepath.Rel(s.root, p)
					files[filepath.ToSlash(rel)] = p
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
		}
	}
	for rel, path := range s.extra {
		files[rel] = path
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no license files found; review this dependency before distribution")
	}
	return files, nil
}
func bundle(out string, packages []pkg) error {
	sources := map[string]*source{}
	versions := map[string]string{}
	var mainRoot string
	ensure := func(name, version, root string) *source {
		s := sources[name]
		if s == nil {
			s = &source{root: root, dirs: map[string]bool{}, extra: map[string]string{}}
			sources[name] = s
			versions[name] = version
		}
		return s
	}
	for _, p := range packages {
		var s *source
		if p.Standard {
			s = ensure("go", runtime.Version(), runtime.GOROOT())
		} else if p.Module != nil {
			m := p.Module
			if m.Replace != nil {
				return fmt.Errorf("module replacement %s requires distribution review", m.Path)
			}
			name := "modules/" + m.Path
			version := m.Version
			if m.Main {
				name = "millivolt"
				mainRoot = m.Dir
				version = millivolt.Version()
			}
			s = ensure(name, version, m.Dir)
		} else {
			return fmt.Errorf("no module authority for %s", p.ImportPath)
		}
		if err := addAncestors(s, p.Dir); err != nil {
			return err
		}
		for _, embedded := range p.EmbedFiles {
			if licenseName(filepath.Base(embedded)) {
				path := filepath.Join(p.Dir, embedded)
				rel, _ := filepath.Rel(s.root, path)
				s.extra[filepath.ToSlash(rel)] = path
			}
		}
	}
	if mainRoot == "" || sources["go"] == nil {
		return fmt.Errorf("missing application or Go toolchain in graph")
	}
	sources["millivolt"].extra["THIRD_PARTY_NOTICES.md"] = filepath.Join(mainRoot, "THIRD_PARTY_NOTICES.md")
	// Collect everything before creating output, so a missing module notice fails closed.
	names := make([]string, 0, len(sources))
	all := map[string]map[string]string{}
	for name, s := range sources {
		files, err := collect(s)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		names = append(names, name)
		all[name] = files
	}
	slices.Sort(names)
	if err := os.Mkdir(out, 0755); err != nil {
		return err
	}
	var manifest []entry
	for _, name := range names {
		e := entry{Name: name, Version: versions[name]}
		paths := make([]string, 0, len(all[name]))
		for path := range all[name] {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		for _, path := range paths {
			data, err := os.ReadFile(all[name][path])
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(data)) == 0 {
				return fmt.Errorf("%s: empty license", path)
			}
			dest := filepath.Join(out, filepath.FromSlash(name), filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dest, data, 0644); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			e.Files = append(e.Files, notice{Path: path, SHA256: hex.EncodeToString(sum[:])})
		}
		manifest = append(manifest, e)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "manifest.json"), append(data, '\n'), 0644)
}
