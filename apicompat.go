//core logic

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	tmpModRoot, err := copyModuleToTempDir(repoRoot, m.modPath, m.modRoot)
	if err != nil {
		return moduleInfo{}, err
	}
	defer func() {
		if rerr := os.RemoveAll(tmpModRoot); err == nil && rerr != nil {
			err = fmt.Errorf("removing temporary module directory: %v", rerr)
		}
	}()
	tmpLoadDir, tmpGoModData, tmpGoSumData, pkgPaths, prepareDiagnostics, err := prepareLoadDir(ctx, m.goModFile, m.modPath, tmpModRoot, version, false)
	if err != nil {
		return moduleInfo{}, err
	}
	defer func() {
		if rerr := os.RemoveAll(tmpLoadDir); err == nil && rerr != nil {
			err = fmt.Errorf("removing temporary load directory: %v", rerr)
		}
	}()

	var loadDiagnostics []string
	m.pkgs, loadDiagnostics, err = loadPackages(ctx, m.modPath, tmpModRoot, tmpLoadDir, tmpGoModData, tmpGoSumData, pkgPaths)
	if err != nil {
		return moduleInfo{}, err
	}

	m.diagnostics = append(m.diagnostics, prepareDiagnostics...)
	m.diagnostics = append(m.diagnostics, loadDiagnostics...)
	if packagesHaveErrors(m.pkgs) {
		// m.goModFile was parsed with ParseLax, which ignores replace and
		// exclude directives.
		modFile, err := modfile.Parse(m.goModPath, m.goModData, nil)
		if err == nil {
			if d := constructReplaceExcludeDiagnostic(modFile); d != "" {
				m.diagnostics = append(m.diagnostics, d)
			}
		}
	}

	highestVersion, err := findSelectedVersion(ctx, tmpLoadDir, m.modPath)
	if err != nil {
		return moduleInfo{}, err
	}

	if highestVersion != "" {
		// A version of the module is included in the transitive dependencies.
		// Add it to the moduleInfo so that the release report stage can use it
		// in verifying the version or suggestion a new version, depending on
		// whether the user provided a version already.
		m.highestTransitiveVersion = highestVersion
	}

	retracted, err := loadRetractions(ctx, tmpLoadDir)
	if err != nil {
		return moduleInfo{}, err
	}
	m.diagnostics = append(m.diagnostics, retracted...)

	return m, nil
}

