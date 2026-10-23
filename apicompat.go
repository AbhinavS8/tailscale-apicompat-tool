//core logic

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"go/build"
	"errors"
	"os/exec"
	"strings"
	"path/filepath"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	// "golang.org/x/tools/go/packages"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("gorelease: ")

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), "env", append(os.Environ(), "GO111MODULE=on"))
	success, err := runCheck(ctx, os.Stdout, wd, os.Args[1:])
	if err != nil {
		if _, ok := err.(*usageError); ok {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		} else {
			log.Fatal(err)
		}
	}
	if !success {
		os.Exit(1)
	}
}

// it writes to w instead of os.Stdout and returns an error instead of
// exiting (for testing purposes)
func runCheck(ctx context.Context, w io.Writer, dir string, args []string) (success bool, err error) {

	// Validate arguments and flags.
	// suppress errors so we can handle it ourselves in testing instead of
	// printing to stderr
	fs := flag.NewFlagSet("apicompat", flag.ContinueOnError)
	fs.Usage = func() {}
	fs.SetOutput(io.Discard)
	var baseVersion string //not checking for module path, assuming only version passed in
	fs.StringVar(&baseVersion, "base", "", "previous version to compare against")
	if err := fs.Parse(args); err != nil {
		return false, &usageError{err: err}
	}

	if len(fs.Args()) > 0 {
		return false, usageErrorf("no arguments allowed")
	}

	modRoot, err := findModuleRoot(dir)
	if err != nil {
		return false, err
	}

	// Load packages for the version to be released from the local directory.
	release, err := loadLocalModule(ctx, modRoot)
	if err != nil {
		return false, err
	}

	fmt.Println(release)

	return true, nil
}

// findModuleRoot finds the root directory of the module that contains dir.
func findModuleRoot(dir string) (string, error) {
	d := filepath.Clean(dir)
	for {
		if fi, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("%s: cannot find go.mod file", dir)
}

type moduleInfo struct {
	modRoot                  string // module root directory
	repoRoot                 string // repository root directory (may be "")
	modPath                  string // module path in go.mod
	version                  string // resolved version or "none"
	versionQuery             string // a query like "latest" or "dev-branch", if specified
	versionInferred          bool   // true if the version was unspecified and inferred
	highestTransitiveVersion string // version of the highest transitive self-dependency (cycle)
	modPathMajor             string // major version suffix like "/v3" or ".v2"
	tagPrefix                string // prefix for version tags if module not in repo root

	goModPath string        // file path to go.mod
	goModData []byte        // content of go.mod
	goSumData []byte        // content of go.sum
	goModFile *modfile.File // parsed go.mod file

	diagnostics []string // problems not related to loading specific packages
	// pkgs        []*packages.Package // loaded packages with type information

}

func loadLocalModule(ctx context.Context, modRoot string) (m moduleInfo, err error) {

	m = moduleInfo{
		modRoot:   modRoot,
		goModPath: filepath.Join(modRoot, "go.mod"),
	}

	m.goModData, err = os.ReadFile(m.goModPath)
	if err != nil {
		return moduleInfo{}, err
	}
	m.goModFile, err = modfile.ParseLax(m.goModPath, m.goModData, nil)
	if err != nil {
		return moduleInfo{}, err
	}
	if m.goModFile.Module == nil {
		return moduleInfo{}, fmt.Errorf("%s: module directive is missing", m.goModPath)
	}

	m.modPath = m.goModFile.Module.Mod.Path
	if err := module.CheckPath(m.modPath); err != nil {
		return moduleInfo{}, err
	}

	// Load the module's packages.
	// We pack the module into a zip file and extract it to a temporary directory
	// as if it were published and downloaded. We'll detect any errors that would
	// occur (for example, invalid file names). We avoid loading it as the
	// main module.
	tmpModRoot, err := copyModuleToTempDir(m.modRoot)
	if err != nil {
		return moduleInfo{}, err
	}
	defer func() {
		if rerr := os.RemoveAll(tmpModRoot); err == nil && rerr != nil {
			err = fmt.Errorf("removing temporary module directory: %v", rerr)
		}
	}()

	tmpLoadDir, tmpGoModData, tmpGoSumData, pkgPaths, prepareDiagnostics, err := prepareLoadDir(ctx, m.goModFile, m.modPath, tmpModRoot, false)
	if err != nil {
		return moduleInfo{}, err
	}
	defer func() {
		if rerr := os.RemoveAll(tmpLoadDir); err == nil && rerr != nil {
			err = fmt.Errorf("removing temporary load directory: %v", rerr)
		}
	}()

	fmt.Println(tmpLoadDir, tmpGoModData, tmpGoSumData, pkgPaths, prepareDiagnostics)
	return m, nil
}

func copyModuleToTempDir(modRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "apicompat")
	if err != nil {
		return "", err
	}

	err = filepath.WalkDir(modRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(modRoot, path)

		if d.IsDir() {
			// Skip vendor, VCS dirs, and submodules
			name := d.Name()
			if name == "vendor" || name == ".git" || name == ".hg" {
				return filepath.SkipDir
			}
			// Skip submodules (has its own go.mod)
			if rel != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return os.MkdirAll(filepath.Join(dir, rel), 0777)
		}

		// Only copy regular files (no symlinks)
		if !d.Type().IsRegular() {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0666)
	})

	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// prepareLoadDir creates a temporary directory and a go.mod file that requires
// the module being loaded. go.sum is copied if present. It also creates a .go
// file that imports every package in the given modPath. This temporary module
// is useful for two reasons. First, replace and exclude directives from the
// target module aren't applied, so we have the same view as a dependent module.
// Second, we can run commands like 'go get' without modifying the original
// go.mod and go.sum files.
//
// modFile is the pre-parsed go.mod file. If non-nil, its requirements and
// go version will be copied so that incomplete and out-of-date requirements
// may be reported later.
//
// modPath is the module's path.
//
// modRoot is the module's root directory.
//
// version is the version of the module being loaded. If must be canonical
// for modules loaded from the cache. Otherwise, it may be empty (for example,
// when no release version is proposed).
//
// cached indicates whether the module is being loaded from the module cache.
// If cached is true, then the module lives in the cache at
// $GOMODCACHE/$modPath@$version/. Its go.mod file is at
// $GOMODCACHE/cache/download/$modPath/@v/$version.mod. It must be referenced
// with a simple require. A replace directive won't work because it may not have
// a go.mod file in modRoot.
// If cached is false, then modRoot is somewhere outside the module cache
// (ex /tmp). We'll reference it with a local replace directive. It must have a
// go.mod file in modRoot.
//
// dir is the location of the temporary directory.
//
// goModData and goSumData are the contents of the go.mod and go.sum files,
// respectively.
//
// pkgPaths are the import paths of the module being loaded, including the path
// to any main packages (as if they were importable).
func prepareLoadDir(ctx context.Context, modFile *modfile.File, modPath, modRoot string, cached bool) (dir string, goModData, goSumData []byte, pkgPaths []string, diagnostics []string, err error) {
	defer func() {
		if err != nil {
			if cached {
				err = fmt.Errorf("preparing to load packages for %s@%s: %w", modPath, err)
			} else {
				err = fmt.Errorf("preparing to load packages for %s: %w", modPath, err)
			}
		}
	}()

	dir, err = os.MkdirTemp("", "apicompat-load")
	if err != nil {
		return "", nil, nil, nil, nil, err
	}

	f := &modfile.File{}
	f.AddModuleStmt("apicompat-load-module")
	f.AddRequire(modPath, "")
	if !cached {
		// Replace all versions of modPath (not just the specific version
		// required above) with the local module directory. A version-scoped
		// replace only takes effect when minimal version selection picks that
		// exact version. If the module appears in its own build graph as a
		// transitive dependency at a higher version (an import cycle across
		// modules, e.g. a -> b -> a), MVS selects that higher version, the
		// version-scoped replace is silently ignored, and the published
		// module is loaded from the cache instead of the local directory.
		// That makes the local module's API look like the older published
		// version and reports spurious changes. A wildcard replace pins the
		// local directory regardless of the selected version.
		f.AddReplace(modPath, "", modRoot, "")
	}
	if modFile != nil {
		if modFile.Go != nil {
			f.AddGoStmt(modFile.Go.Version)
		}
		for _, r := range modFile.Require {
			f.AddRequire(r.Mod.Path, r.Mod.Version)
		}
	}
	goModData, err = f.Format()
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), goModData, 0666); err != nil {
		return "", nil, nil, nil, nil, err
	}

	goSumData, err = os.ReadFile(filepath.Join(modRoot, "go.sum"))
	if err != nil && !os.IsNotExist(err) {
		return "", nil, nil, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), goSumData, 0666); err != nil {
		return "", nil, nil, nil, nil, err
	}

	// Add a .go file with requirements, so that `go get` won't blat
	// requirements.
	fakeImports := &strings.Builder{}
	fmt.Fprint(fakeImports, "package tmp\n")
	imps, err := collectImportPaths(modPath, modRoot)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	for _, imp := range imps {
		fmt.Fprintf(fakeImports, "import _ %q\n", imp)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp.go"), []byte(fakeImports.String()), 0666); err != nil {
		return "", nil, nil, nil, nil, err
	}

	// Add missing requirements.
	cmd := exec.CommandContext(ctx, "go", "get", "-d", ".")
	cmd.Env = copyEnv(ctx, cmd.Env)
	cmd.Dir = dir
	if _, err := cmd.Output(); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("looking for missing dependencies: %w", cleanCmdError(err))
	}

	// Report new requirements in go.mod.
	goModPath := filepath.Join(dir, "go.mod")
	loadReqs := func(data []byte) (reqs []module.Version, err error) {
		modFile, err := modfile.ParseLax(goModPath, data, nil)
		if err != nil {
			return nil, err
		}
		for _, r := range modFile.Require {
			reqs = append(reqs, r.Mod)
		}
		return reqs, nil
	}

	oldReqs, err := loadReqs(goModData)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	newGoModData, err := os.ReadFile(goModPath)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	newReqs, err := loadReqs(newGoModData)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}

	oldMap := make(map[module.Version]bool)
	for _, req := range oldReqs {
		oldMap[req] = true
	}
	var missing []module.Version
	for _, req := range newReqs {
		// Ignore cyclic imports, since a module never needs to require itself.
		if req.Path == modPath {
			continue
		}
		if !oldMap[req] {
			missing = append(missing, req)
		}
	}

	if len(missing) > 0 {
		var missingReqs []string
		for _, m := range missing {
			missingReqs = append(missingReqs, m.String())
		}
		diagnostics = append(diagnostics, fmt.Sprintf("go.mod: the following requirements are needed\n\t%s\nRun 'go mod tidy' to add missing requirements.", strings.Join(missingReqs, "\n\t")))
		return dir, goModData, goSumData, imps, diagnostics, nil
	}

	// Cached modules may have no go.sum.
	// We skip comparison because a downloaded module is outside the user's
	// control.
	if !cached {
		// Check if 'go get' added new hashes to go.sum.
		goSumPath := filepath.Join(dir, "go.sum")
		newGoSumData, err := os.ReadFile(goSumPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", nil, nil, nil, nil, err
			}
			// If the sum doesn't exist, that's ok: we'll treat "no go.sum" like
			// "empty go.sum".
		}

		if !sumsMatchIgnoringPath(string(goSumData), string(newGoSumData), modPath) {
			diagnostics = append(diagnostics, "go.sum: one or more sums are missing. Run 'go mod tidy' to add missing sums.")
		}
	}

	return dir, goModData, goSumData, imps, diagnostics, nil
}

func cleanCmdError(err error) error {
	if xerr, ok := err.(*exec.ExitError); ok {
		if stderr := strings.TrimSpace(string(xerr.Stderr)); stderr != "" {
			return errors.New(stderr)
		}
	}
	return err
}

func copyEnv(ctx context.Context, current []string) []string {
	env, ok := ctx.Value("env").([]string)
	if !ok {
		return current
	}
	clone := make([]string, len(env))
	copy(clone, env)
	return clone
}

// sumsMatchIgnoringPath checks whether the two sums match. It ignores any lines
// which contains the given modPath.
func sumsMatchIgnoringPath(sum1, sum2, modPathToIgnore string) bool {
	lines1 := make(map[string]bool)
	for _, line := range strings.Split(string(sum1), "\n") {
		if line == "" {
			continue
		}
		lines1[line] = true
	}
	for _, line := range strings.Split(string(sum2), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 1 {
			panic(fmt.Sprintf("go.sum malformed: unexpected line %s", line))
		}
		if parts[0] == modPathToIgnore {
			continue
		}

		if !lines1[line] {
			return false
		}
	}

	lines2 := make(map[string]bool)
	for _, line := range strings.Split(string(sum2), "\n") {
		if line == "" {
			continue
		}
		lines2[line] = true
	}
	for _, line := range strings.Split(string(sum1), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 1 {
			panic(fmt.Sprintf("go.sum malformed: unexpected line %s", line))
		}
		if parts[0] == modPathToIgnore {
			continue
		}

		if !lines2[line] {
			return false
		}
	}

	return true
}

// collectImportPaths visits the given root and traverses its directories
// recursively, collecting the import paths of all importable packages in each
// directory along the way.
//
// modPath is the module path.
// root is the root directory of the module to collect imports for (the root
// of the modPath module).
//
// Note: the returned importPaths will include main if it exists in root.
func collectImportPaths(modPath, root string) (importPaths []string, _ error) {
	err := filepath.Walk(root, func(walkPath string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Avoid .foo, _foo, and testdata subdirectory trees.
		if !fi.IsDir() {
			return nil
		}
		base := filepath.Base(walkPath)
		if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata" || base == "internal" {
			return filepath.SkipDir
		}

		p, err := build.Default.ImportDir(walkPath, 0)
		if err != nil {
			if nogoErr := (*build.NoGoError)(nil); errors.As(err, &nogoErr) {
				// No .go files found in directory. That's ok, we'll keep
				// searching.
				return nil
			}
			return err
		}

		// Construct the import path.
		importPath := path.Join(modPath, filepath.ToSlash(trimFilePathPrefix(p.Dir, root)))
		importPaths = append(importPaths, importPath)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing packages in %s: %v", root, err)
	}

	return importPaths, nil
}