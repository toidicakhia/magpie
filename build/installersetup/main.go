// installersetup builds magpie's Windows installer: the same release app
// that `make release-windows` puts in dist, then the setup program in
// build/windows/installer with the app gzipped and appended to it. The app
// is built with packed assets, ConPTY and the terminal companion, and with
// the exe's icon and version resources, so the installed magpie is the one
// the site's download would have been.
//
// Each architecture has its own setup: the appended exe is that
// architecture's, so there is nothing to choose at install time, and no
// download at install time either.
//
// Usage: go run ./build/installersetup [-arch amd64|arm64] [-version vX] [-out dist/setup.exe]
package main

import (
	"compress/gzip"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// magic must match the setup's: it ends the appended stream.
const magic = "MAGPIE-SETUP-1"

var safeVersion = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

func main() {
	arch := flag.String("arch", goarch(), "windows architecture to build for")
	version := flag.String("version", "", "version to stamp; default: git describe")
	out := flag.String("out", "", "where to write the setup exe")
	flag.Parse()
	if err := run(*arch, *version, *out); err != nil {
		fmt.Fprintln(os.Stderr, "installersetup:", err)
		os.Exit(1)
	}
}

func run(arch, version, out string) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported architecture %q: use amd64 or arm64", arch)
	}
	if version == "" {
		version = describe()
	}
	if safeVersion.MatchString(version) {
		return fmt.Errorf("version %q has characters a version isn't spelled with", version)
	}
	v := winVersion(version)
	if out == "" {
		out = filepath.Join("dist", "magpie-setup-"+version+"-windows-"+arch+".exe")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}

	// the app's assets, ConPTY and launcher are the release's
	step("packing the UI's assets")
	if err := cmd(nil, "go", "run", "./build/assetpack", "internal/gui/assets", "internal/gui/assets.zip"); err != nil {
		return err
	}
	step("fetching ConPTY")
	if err := cmd(nil, "go", "run", "./build/conpty"); err != nil {
		return err
	}
	launcher := filepath.Join("internal", "update", "launcher-"+arch+".com")
	defer os.Remove(launcher)
	step("building the " + arch + " terminal companion")
	if err := cmd([]string{"CGO_ENABLED=0", "GOOS=windows", "GOARCH=" + arch},
		"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", launcher, "./internal/update/launcher"); err != nil {
		return err
	}

	// go-winres writes a .syso in --out's folder, which only the package
	// built from that folder links; both binaries need their own.
	appRes := filepath.Join("rsrc_windows_"+arch+".syso")
	defer os.Remove(appRes)
	step("stamping the app's resources")
	if err := cmd(nil, "go", "run", "github.com/tc-hib/go-winres@v0.3.3", "make",
		"--in", "build/windows/winres.json", "--arch", arch, "--out", "rsrc",
		"--file-version", v, "--product-version", v); err != nil {
		return err
	}
	app := filepath.Join("dist", "magpie-windows-"+arch+".exe")
	step("building the app → " + app)
	if err := cmd([]string{"CGO_ENABLED=0", "GOOS=windows", "GOARCH=" + arch},
		"go", "build", "-tags", "production,comlauncher,packedassets,conptypair", "-trimpath",
		"-ldflags", "-s -w -X main.version="+version+" -H windowsgui", "-o", app, "."); err != nil {
		return err
	}

	setupRes := filepath.Join("build", "windows", "installer", "rsrc_windows_"+arch+".syso")
	defer os.Remove(setupRes)
	step("stamping setup's resources")
	if err := cmd(nil, "go", "run", "github.com/tc-hib/go-winres@v0.3.3", "make",
		"--in", "build/windows/installer/winres.json", "--arch", arch,
		"--out", filepath.Join("build", "windows", "installer", "rsrc"),
		"--file-version", v, "--product-version", v); err != nil {
		return err
	}
	step("building setup → " + out)
	if err := cmd([]string{"CGO_ENABLED=0", "GOOS=windows", "GOARCH=" + arch},
		"go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags", "-s -w -X main.version="+version+" -H windowsgui", "-o", out,
		"./build/windows/installer"); err != nil {
		return err
	}
	step("appending the app to setup")
	if err := appendPayload(out, app); err != nil {
		return err
	}
	fmt.Println("  " + out)
	return nil
}

// appendPayload adds the gzipped app, then its offset and the magic, to
// the setup exe. Windows runs a PE with bytes after its last section
// without minding them, and setup reads its own file to get them back.
func appendPayload(setup, app string) error {
	out, err := os.OpenFile(setup, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	st, err := out.Stat()
	if err != nil {
		out.Close()
		return err
	}
	off := st.Size()
	in, err := os.Open(app)
	if err != nil {
		out.Close()
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	cerr := zw.Close()
	in.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		var b [8 + len(magic)]byte
		binary.LittleEndian.PutUint64(b[:8], uint64(off))
		copy(b[8:], magic)
		_, err = out.Write(b[:])
	}
	return errors.Join(err, out.Close())
}

// cmd runs a build step with its output going where the caller's does.
func cmd(env []string, name string, args ...string) error {
	c := exec.Command(name, args...)
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func step(s string) { fmt.Println("  " + s) }

func goarch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	default:
		return "amd64"
	}
}

// describe is the version the Makefile stamps when none is given, kept
// to what a version is spelled with (the Makefile's VERSION).
func describe() string {
	c := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	b, err := c.Output()
	if err != nil {
		return "dev"
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "dev"
	}
	return safeVersion.ReplaceAllString(v, "")
}

// winVersion is the numeric part git describe's version starts with, the
// number Windows shows in an exe's resources. A build of one's own has
// none, so it is 0.0.0.
func winVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	end := 0
	for i := 0; i < len(v); i++ {
		if v[i] >= '0' && v[i] <= '9' || v[i] == '.' {
			end = i + 1
			continue
		}
		break
	}
	v = strings.Trim(strings.TrimSuffix(v[:end], "."), ".")
	for strings.Count(v, ".") < 2 {
		v += ".0"
	}
	if v == "" {
		return "0.0.0"
	}
	return v
}
