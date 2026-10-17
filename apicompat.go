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
	// tmpLoadDir, tmpGoModData, tmpGoSumData, pkgPaths, prepareDiagnostics, err := prepareLoadDir(ctx, m.goModFile, m.modPath, tmpModRoot, version, false)
	// if err != nil {
	// 	return moduleInfo{}, err
	// }
	// defer func() {
	// 	if rerr := os.RemoveAll(tmpLoadDir); err == nil && rerr != nil {
	// 		err = fmt.Errorf("removing temporary load directory: %v", rerr)
	// 	}
	// }()

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
