// A swiss army knife of functions for working with Golang projects.
//
// A collection of functions for building, formatting, testing, linting and scanning
// your Go project for vulnerabilities.

package main

import (
	"context"
	"dagger/golang/internal/dagger"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/disaster37/dagger-library-go/lib/helper"
	"golang.org/x/mod/modfile"
)

const (
	goMod     = "go.mod"
	goWorkDir = "/src"
	netrcPath = "/root/.netrc"
)

// Enables support for accessing private Go modules as project dependencies
type GoPrivate struct {
	// A .netrc configuration file that supports auto-login to remote machines
	// (hosts) containing the private Go modules for download
	// +private
	Netrc *dagger.Netrc

	// A list of modules that are private and should not be retrieved from
	// the public Go module mirror. Ultimately this will be controlled through
	// the GOPRIVATE environment variable
	// +private
	Modules []string
}

// Golang dagger module
type Golang struct {
	// Base is the image used by all golang dagger functions, defaults to the bookworm base image
	Container *dagger.Container

	// Private Go module support
	// +private
	Private *GoPrivate

	// Version of the go project, defined within the go.mod file
	// +private
	Version string

	// The bin path where to store extra binary file
	// The volume is put on cache
	// +private
	BinPath string
}

// New initializes the golang dagger module
func New(
	ctx context.Context,
	// A custom base image containing an installation of golang. If no image is provided,
	// one is resolved based on the Go version defined within the projects go.mod file. The
	// official Go image is pulled from DockerHub using either the bullseye (< 1.20) or
	// bookworm (> 1.20) variants.
	// +optional
	base *dagger.Container,
	// The golang version to use when no go.mod
	// +optional
	version string,
	// a path to a directory containing the source code
	// +required
	src *dagger.Directory,
) (*Golang, error) {
	expectedVersion, err := inspectModVersion(context.Background(), src)
	if err != nil {
		if version != "" {
			expectedVersion = version
		} else {
			return nil, err
		}
	}
	version = expectedVersion

	if base == nil {
		base = defaultImage(version)
	} else {
		if _, err = base.WithoutEntrypoint().WithExec([]string{"go", "version"}).Sync(ctx); err != nil {
			return nil, err
		}
	}

	golang := &Golang{
		Version:   version,
		Container: base,
	}

	// Ensure cache mounts are configured for any type of image
	golang.Container = golang.mountCaches(ctx).
		WithDirectory(goWorkDir, src).
		WithWorkdir(goWorkDir).
		WithoutEntrypoint()

	return golang, nil
}

func inspectModVersion(ctx context.Context, src *dagger.Directory) (string, error) {
	mod, err := src.File(goMod).Contents(ctx)
	if err != nil {
		return "", err
	}

	f, err := modfile.Parse(goMod, []byte(mod), nil)
	if err != nil {
		return "", err
	}
	return f.Go.Version, nil
}

func (h *Golang) mountCaches(ctx context.Context) *dagger.Container {
	goEnvStdout, err := h.Container.WithExec([]string{"go", "env", "-json"}).Stdout(ctx)
	if err != nil {
		panic(fmt.Sprintf("Error when get go env; %s", err.Error()))
	}
	var goEnv map[string]string
	if err := json.Unmarshal([]byte(goEnvStdout), &goEnv); err != nil {
		panic(fmt.Sprintf("Error when decode go env; %s", err.Error()))
	}

	goCacheEnv := goEnv["GOCACHE"]
	goModCacheEnv := goEnv["GOMODCACHE"]
	goBinCacheEnv := goEnv["GOBIN"]
	if goBinCacheEnv == "" {
		goBinCacheEnv = fmt.Sprintf("%s/bin", goEnv["GOPATH"])
	}

	gomod := dag.CacheVolume("gomod")
	gobuild := dag.CacheVolume("gobuild")
	gobin := dag.CacheVolume("gobin")

	h.BinPath = goBinCacheEnv

	h.Container = h.Container.
		WithMountedCache(goModCacheEnv, gomod).
		WithMountedCache(goCacheEnv, gobuild).
		WithMountedCache(goBinCacheEnv, gobin)

	return h.Container
}

// Echoes the version of go defined within a projects go.mod file.
// It expects the go.mod file to be located within the root of the project
func (g *Golang) ModVersion() string {
	return g.Version
}

// GoBin return the Go bin path
// It can be usefull to add bin on this because of cache volume
func (g *Golang) GoBin() string {
	return g.BinPath
}

func defaultImage(version string) *dagger.Container {
	return dag.Container().From(fmt.Sprintf("golang:%s", version))
}

// Enable private Go module support by dynamically constructing a .netrc auto-login
// configuration file. Each call will append a new auto-login configuration
func (g *Golang) WithPrivate(
	ctx context.Context,
	// the remote machine name
	// +required
	machine string,
	// a user on the remote machine that can login
	// +required
	username *dagger.Secret,
	// a token (or password) used to login into a remote machine by
	// the identified user
	// +required
	password *dagger.Secret,
	// a list of Go module paths that will be treated as private by Go
	// through the GOPRIVATE environment variable
	// +required
	modules []string,
) *Golang {
	if g.Private == nil {
		g.Private = &GoPrivate{
			Netrc: dag.Netrc(dagger.NetrcOpts{Format: dagger.NetrcFormatCompact}),
		}
	}

	g.Private.Netrc = g.Private.Netrc.WithLogin(machine, username, password)
	g.Private.Modules = append(g.Private.Modules, modules...)
	return g
}

// Enable private Go module support by loading an existing .netrc auto-login configuration
// file. Each call will append a new auto-login configuration
func (g *Golang) WithPrivateLoad(
	ctx context.Context,
	// a path to a .netrc auto-login configuration file
	// +required
	cfg *dagger.File,
	// a list of Go module paths that will be treated as private by Go
	// through the GOPRIVATE environment variable
	// +required
	modules []string,
) *Golang {
	if g.Private == nil {
		g.Private = &GoPrivate{
			Netrc: dag.Netrc(dagger.NetrcOpts{Format: dagger.NetrcFormatCompact}),
		}
	}

	g.Private.Netrc = g.Private.Netrc.WithFile(cfg)
	g.Private.Modules = append(g.Private.Modules, modules...)
	return g
}

func (g *Golang) enablePrivateModules() *dagger.Container {
	if g.Private == nil {
		return g.Container
	}

	return g.Container.
		WithEnvVariable("GOPRIVATE", strings.Join(g.Private.Modules, ",")).
		WithEnvVariable("NETRC", netrcPath).
		WithMountedSecret(netrcPath, g.Private.Netrc.AsSecret())
}

// Build a static binary from a Go project using the provided configuration.
// A directory is returned containing the built binary.
func (g *Golang) Build(
	// the path to the main.go file of the project
	// +optional
	main string,
	// the name of the built binary
	// +optional
	out string,
	// the target operating system
	// +optional
	os string,
	// the target architecture
	// +optional
	arch string,
	// flags to configure the linking during a build, by default sets flags for
	// generating a release binary
	// +optional
	// +default=["-s", "-w"]
	ldflags []string,
) *dagger.Directory {
	if os == "" {
		os = runtime.GOOS
	}

	if arch == "" {
		arch = runtime.GOARCH
	}

	cmd := []string{"go", "build", "-ldflags", strings.Join(ldflags, " ")}
	if out != "" {
		cmd = append(cmd, "-o", out)
	}

	if main != "" {
		cmd = append(cmd, main)
	}

	ctr := g.Container
	if g.Private != nil {
		ctr = g.enablePrivateModules()
	}

	return ctr.
		WithEnvVariable("CGO_ENABLED", "0").
		WithEnvVariable("GOOS", os).
		WithEnvVariable("GOARCH", arch).
		WithExec(cmd).
		Directory(goWorkDir)
}

// Execute tests defined within the target project, ignores benchmarks by default
func (g *Golang) Test(
	ctx context.Context,
	// if only short running tests should be executed
	// +optional
	short bool,
	// if the tests should be executed out of order
	// +optional
	shuffle bool,
	// run select tests only, defined using a regex
	// +optional
	run string,
	// skip select tests, defined using a regex
	// +optional
	skip string,
	// Run test with gotestsum
	// +optional
	withGotestsum bool,
	// Path to test
	// +optional
	path string,
) *dagger.File {

	ctr := g.Container

	var cmd []string
	testPath := "./..."
	if path != "" {
		testPath = path
	}

	if withGotestsum {
		if _, err := ctr.WithExec([]string{"gotestsum", "--version"}).Sync(ctx); err != nil {
			ctr = ctr.WithExec(helper.ForgeCommand("go install gotest.tools/gotestsum@latest"))
		}
		cmd = []string{"gotestsum", "--format", "testname", "--"}
	} else {
		cmd = []string{"go", "test", "-v"}
	}
	cmd = append(cmd, "-p=1", "-count=1", "-vet=off", "-timeout=60m", "-covermode=atomic", "-coverprofile=coverage.out.tmp", testPath)
	if short {
		cmd = append(cmd, "-short")
	}

	if shuffle {
		cmd = append(cmd, "-shuffle=on")
	}

	if run != "" {
		cmd = append(cmd, []string{"-run", run}...)
	}

	if skip != "" {
		cmd = append(cmd, []string{"-skip", skip}...)
	}

	if g.Private != nil {
		ctr = g.enablePrivateModules()
	}

	return ctr.
		WithExec(cmd).
		WithExec(helper.ForgeScript(`cat coverage.out.tmp | grep -v "_generated.*.go" > coverage.out`)).
		File("coverage.out")
}

// Execute tests defined within the target project, ignores benchmarks by default
// Debug test with dlv
func (g *Golang) DebugTest(
	ctx context.Context,
	// run select tests only, defined using a regex
	// +optional
	run string,
) *dagger.Service {

	ctr := g.Container

	if _, err := ctr.WithExec([]string{"dlv", "version"}).Sync(ctx); err != nil {
		ctr = ctr.
			WithExec(helper.ForgeScript(`
set -e

go install github.com/acroca/go-symbols@latest &&\
go install github.com/cweill/gotests/gotests@latest &&\
go install github.com/davidrjenni/reftools/cmd/fillstruct@latest &&\
go install github.com/haya14busa/goplay/cmd/goplay@latest &&\
go install github.com/stamblerre/gocode@latest &&\
mv /go/bin/gocode /go/bin/gocode-gomod &&\
go install github.com/mdempsky/gocode@latest &&\
go install github.com/ramya-rao-a/go-outline@latest &&\
go install github.com/rogpeppe/godef@latest &&\
go install github.com/sqs/goreturns@latest &&\
go install github.com/uudashr/gopkgs/v2/cmd/gopkgs@latest &&\
go install github.com/zmb3/gogetdoc@latest &&\
go install honnef.co/go/tools/cmd/staticcheck@latest &&\
go install golang.org/x/tools/cmd/gorename@latest &&\
go install github.com/go-delve/delve/cmd/dlv@latest &&\
go install golang.org/x/tools/gopls@latest
		`))
	}

	cmd := []string{
		"dlv",
		"test",
		"--listen=:4000",
		"--log=true",
		"--headless=true",
		"--accept-multiclient",
		"--api-version=2",
	}

	if run != "" {
		cmd = append(cmd, "--", "-test.run", run)
	}

	return ctr.
		WithExposedPort(4000).
		WithEntrypoint(cmd).
		AsService()
}

// Execute benchmarks defined within the target project, excludes all other tests
func (g *Golang) Bench(
	ctx context.Context,
	// print memory allocation statistics for benchmarks
	// +optional
	// +default=true
	memory bool,
	// the time.Duration each benchmark should run for
	// +optional
	// +default="5s"
	time string,
) (string, error) {
	cmd := []string{"go", "test", "-bench=.", "-benchtime", time, "-run=^#", "./..."}
	if memory {
		cmd = append(cmd, "-benchmem")
	}

	ctr := g.Container
	if g.Private != nil {
		ctr = g.enablePrivateModules()
	}

	return ctr.WithExec(cmd).Stdout(ctx)
}

// Scans the target project for vulnerabilities using govulncheck
func (g *Golang) Vulncheck(ctx context.Context) (string, error) {
	if g.Version == "1.17" {
		return "", fmt.Errorf("govulncheck supports go versions 1.18 and higher")
	}

	ctr := g.Container
	if _, err := ctr.WithExec([]string{"govulncheck", "--version"}).Sync(ctx); err != nil {
		tag, err := dag.Github().GetLatestRelease("golang/vuln").Tag(ctx)
		if err != nil {
			return "", err
		}

		ctr = ctr.WithExec([]string{"go", "install", "golang.org/x/vuln/cmd/govulncheck@" + tag})
	}

	if g.Private != nil {
		ctr = g.enablePrivateModules()
	}

	return ctr.
		WithExec([]string{"govulncheck", "./..."}).
		Stdout(ctx)
}

// Lint the target project using golangci-lint
func (g *Golang) Lint(
	ctx context.Context,
) (string, error) {

	ctr := g.Container
	if _, err := ctr.WithExec([]string{"golangci-lint", "version"}).Sync(ctx); err != nil {
		tag, err := dag.Github().GetLatestRelease("golangci/golangci-lint").Tag(ctx)
		if err != nil {
			return "", err
		}

		// Install using the recommended approach: https://golangci-lint.run/welcome/install/
		cmd := []string{
			"curl",
			"-sSfL",
			"https://raw.githubusercontent.com/golangci/golangci-lint/main/install.sh",
			"|",
			"sh",
			"-s",
			"--",
			"-b",
			"$(go env GOPATH)/bin",
			tag,
		}
		ctr = ctr.WithExec([]string{"bash", "-c", strings.Join(cmd, " ")})
	}

	cmd := []string{
		"golangci-lint",
		"run",
		"--timeout",
		"5m",
		"--output.text.colors",
	}

	if g.Private != nil {
		ctr = g.enablePrivateModules()
	}

	return ctr.WithExec(cmd).Stdout(ctx)
}

// Format the source code within a target project using gofumpt. Formatted code must be
// copied back onto the host.`
func (g *Golang) Format(ctx context.Context) (*dagger.Directory, error) {
	ctr := g.Container
	if _, err := ctr.WithExec([]string{"gofumpt", "-version"}).Sync(ctx); err != nil {
		tag, err := dag.Github().GetLatestRelease("mvdan/gofumpt").Tag(ctx)
		if err != nil {
			return nil, err
		}

		ctr = ctr.WithExec([]string{"go", "install", "mvdan.cc/gofumpt@" + tag})
	}

	cmd := []string{"gofumpt", "-w", "-d", "."}

	return ctr.WithExec(cmd, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny}).Directory(goWorkDir), nil
}

// WithSource permit to update the current source on sdk container
func (h *Golang) WithSource(
	// The source directory
	// +required
	src *dagger.Directory,
) *Golang {
	h.Container = h.Container.WithDirectory(".", src)
	return h
}
