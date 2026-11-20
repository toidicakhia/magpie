//go:build windows

// setup is magpie's Windows installer: `go run ./build/installersetup`
// (or `make installer-windows`) builds the release app, appends it to this
// program, and the result — magpie-setup-windows-amd64.exe — puts that app
// where magpie installs itself, %LOCALAPPDATA%\Programs\magpie\magpie.exe,
// puts that folder first on the user's PATH, and opens it.
//
// Everything after that is magpie's own, as it is for a download opened
// from Downloads (internal/update's Settle): the app makes its Start menu
// shortcut, its App Paths and Installed apps entries (whose Uninstall runs
// `magpie uninstall --window`), its magpie.com, and updates itself in
// place. So setup does the one thing the app can't do for itself when it
// is started already installed: it puts the files there, and it says
// InstallLocation in HKCU\Software\magpie when the folder isn't the
// default, which is where the app reads the folder the user chose.
//
// The app is appended after this program's own bytes (a payload, then its
// offset and a trailer) rather than embedded with //go:embed: the payload
// isn't in git, and this package has to build for `go vet ./...` and
// `go test ./...` without it. A setup built without the payload says so
// and changes nothing.
//
// Usage: magpie-setup-windows-amd64.exe [/S] [/DIR=<folder>] [/NOLAUNCH]
package main

import (
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/yetone/magpie/internal/update"
)

// version is set at build time, -ldflags "-X main.version=…", as the app's
// is; the file version in the exe's resources is what Windows shows.
var version = "dev"

const (
	// magic ends the setup exe, right after the payload's offset.
	magic = "MAGPIE-SETUP-1"
	// trailer is the offset (8 bytes, little-endian) and magic.
	trailer = 8 + len(magic)

	exeName = "magpie.exe"

	// messageBox's flags and answers, user32's
	mbOK              = 0x0
	mbYesNo           = 0x4
	mbIconError       = 0x10
	mbIconQuestion    = 0x20
	mbIconInformation = 0x40
	mbTopmost         = 0x40000
	idYes             = 6
)

var errCancelled = errors.New("cancelled")

func main() {
	var o options
	if err := parse(os.Args[1:], &o); err != nil {
		fail(o, err)
	}
	if o.help {
		messageBox("magpie setup", usage, mbOK|mbIconInformation)
		return
	}
	if err := install(o); err != nil {
		if errors.Is(err, errCancelled) {
			return
		}
		fail(o, err)
	}
}

const usage = `magpie setup

Installs magpie for this user, in %LOCALAPPDATA%\Programs\magpie, and opens
it. No administrator is needed, and magpie updates itself in place.

  /S            install without asking
  /DIR=<folder> install in <folder> (a magpie folder is made inside it,
                unless it is named magpie), instead of the default
  /NOLAUNCH     install and don't open magpie
  /?            this

Exit code 0 when magpie is installed, 1 when it isn't.`

// fail says why setup stopped: on stderr, and in a box for whoever
// started it by double-clicking.
func fail(o options, err error) {
	fmt.Fprintln(os.Stderr, "magpie setup:", err)
	if !o.silent {
		messageBox("magpie setup", err.Error(), mbOK|mbIconError)
	}
	os.Exit(1)
}

type options struct {
	silent, noLaunch, help bool
	dir                    string
}

func parse(args []string, o *options) error {
	for _, a := range validate(args) {
		low := strings.ToLower(a)
		switch {
		case low == "/s", low == "-s", low == "--silent":
			o.silent = true
		case low == "/nolaunch", low == "--no-launch", low == "--nolaunch":
			o.noLaunch = true
		case low == "/?", low == "-h", low == "--help", low == "/help":
			o.help = true
		case strings.HasPrefix(low, "/dir="), strings.HasPrefix(low, "--dir="):
			o.dir = strings.Trim(a[strings.IndexByte(a, '=')+1:], `"`)
			if o.dir == "" {
				return errors.New("/DIR= needs a folder")
			}
		default:
			return fmt.Errorf("unknown argument %q: /? says what setup takes", a)
		}
	}
	return nil
}

// validate strips the quotes a shell may leave around an argument, and
// drops the empty ones.
func validate(args []string) []string {
	out := args[:0]
	for _, a := range args {
		if a = strings.Trim(strings.TrimSpace(a), `"`); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// install puts the payload at the folder magpie installs itself in, opens
// it there, and records what a download opened later reads.
func install(o options) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	dir, err := folder(o)
	if err != nil {
		return err
	}
	to := filepath.Join(dir, exeName)
	if !o.silent && !confirm(dir, fileVersion(to) != 0) {
		return errCancelled
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// written beside its place and moved in, so a failed copy leaves the
	// app that is there as it was
	staged := to + ".new"
	if err := extract(self, staged); err != nil {
		os.Remove(staged)
		return err
	}
	if now, new := fileVersion(to), fileVersion(staged); now != 0 && new != 0 && new < now {
		os.Remove(staged)
		return fmt.Errorf("a newer magpie, %s, is installed in %s: setup doesn't put an older one over it", versionText(now), dir)
	}
	if err := os.Rename(staged, to); err != nil {
		os.Remove(staged)
		return fmt.Errorf("putting %s in place: %w; quit magpie, if it is running, and run setup again", exeName, err)
	}
	// neither is worth stopping for: magpie runs without its command on
	// PATH, and its updates name the folder it runs from
	if err := addToPath(dir); err != nil && !o.silent {
		messageBox("magpie setup", "magpie is installed, but its folder isn't on your PATH, so a terminal has no `magpie` command yet.\n\n"+err.Error(), mbOK|mbIconError)
	}
	if err := recordDir(dir); err != nil && !o.silent {
		messageBox("magpie setup", "magpie is installed, but the folder couldn't be recorded for a download opened later.\n\n"+err.Error(), mbOK|mbIconError)
	}
	if o.noLaunch {
		return nil
	}
	return launch(to)
}

// folder is where setup installs: the folder asked for (as a folder the
// app's own dialog picked means it), else %LOCALAPPDATA%\Programs\magpie.
func folder(o options) (string, error) {
	if o.dir == "" {
		d := defaultDir()
		if d == "" {
			return "", errors.New("can't tell where this user's programs go: LOCALAPPDATA isn't set")
		}
		return d, nil
	}
	dir := update.InstallDir(o.dir)
	// the app's own rule for a folder it is installed in: one it can
	// write to, and not one downloads land in, whose copy would be taken
	// for a download again
	if err := update.Installable(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// defaultDir is where the app installs itself: the user's own programs
// folder, no administrator (internal/update's installPath).
func defaultDir() string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "Programs", "magpie")
	}
	if p, err := windows.KnownFolderPath(windows.FOLDERID_UserProgramFiles, 0); err == nil && p != "" {
		return filepath.Join(p, "magpie")
	}
	return ""
}

// confirm asks whether to install in dir, in place of what is there when
// something is.
func confirm(dir string, there bool) bool {
	what := "Install magpie for this user?"
	if there {
		what = "Install magpie for this user, in place of the copy there?"
	}
	body := what + "\n\n" + dir + "\n\n" +
		"magpie installs for you alone and needs no administrator. It adds this folder to your PATH, " +
		"updates itself in place, and is removed from Settings › Apps › magpie › Uninstall."
	return messageBox("magpie setup", body, mbYesNo|mbIconQuestion) == idYes
}

// extract writes the payload appended to self to dst: the gzip stream
// between the offset in the trailer and the trailer itself. A setup with
// no payload says so.
func extract(self, dst string) error {
	f, err := os.Open(self)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	off, end, err := payload(f, st.Size())
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(io.NewSectionReader(f, off, end-off))
	if err != nil {
		return fmt.Errorf("this setup has no magpie in it: %w", err)
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(zr, 512<<20))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

var errNoPayload = errors.New("this setup has no magpie in it: build one with `go run ./build/installersetup`")

// payload is where the appended app starts and ends in a setup exe of size
// bytes: the trailer's offset, and the trailer itself.
func payload(f io.ReaderAt, size int64) (off, end int64, err error) {
	if size <= int64(trailer) {
		return 0, 0, errNoPayload
	}
	end = size - int64(trailer)
	b := make([]byte, trailer)
	if _, err := f.ReadAt(b, end); err != nil {
		return 0, 0, err
	}
	if string(b[8:]) != magic {
		return 0, 0, errNoPayload
	}
	off = int64(binary.LittleEndian.Uint64(b[:8]))
	if off <= 0 || off >= end {
		return 0, 0, errNoPayload
	}
	return off, end, nil
}

// addToPath puts dir first in the user's PATH in the registry, where a
// terminal opened from now on takes it up, and tells Explorer
// (internal/update's cliPlace, which the app's own install runs).
func addToPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	v, typ, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	nv := pathFirst(v, dir)
	if nv == v {
		return nil
	}
	// kept REG_EXPAND_SZ, as Windows has it, so %USERPROFILE% in it still works
	if err == registry.ErrNotExist || typ == registry.EXPAND_SZ {
		err = k.SetExpandStringValue("Path", nv)
	} else {
		err = k.SetStringValue("Path", nv)
	}
	if err != nil {
		return fmt.Errorf("adding %s to your PATH: %w", dir, err)
	}
	os.Setenv("PATH", dir+";"+os.Getenv("PATH"))
	settingChanged()
	return nil
}

// settingChanged tells Explorer the environment changed, so what it starts
// next — a terminal from the Start menu — has the new PATH.
func settingChanged() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	env, _ := windows.UTF16PtrFromString("Environment")
	var res uintptr
	procSendMessageTimeout.Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&res)))
}

// pathFirst is the PATH value v with dir first and nowhere else.
func pathFirst(v, dir string) string {
	same := func(d string) bool {
		return strings.EqualFold(strings.TrimRight(strings.TrimSpace(d), `\`), strings.TrimRight(dir, `\`))
	}
	out := []string{dir}
	for _, d := range strings.Split(v, ";") {
		if strings.TrimSpace(d) != "" && !same(d) {
			out = append(out, d)
		}
	}
	return strings.Join(out, ";")
}

// recordDir keeps dir in HKCU\Software\magpie (InstallLocation), the
// folder the app reads as the one its user chose (internal/update's
// writeInstallDir); the default folder is recorded as no folder, which is
// how the app tells it.
func recordDir(dir string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\magpie`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if strings.EqualFold(filepath.Clean(dir), filepath.Clean(defaultDir())) {
		if err := k.DeleteValue("InstallLocation"); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	return k.SetStringValue("InstallLocation", dir)
}

// launch opens the installed app, detached from setup, which exits at once.
func launch(exe string) error {
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening %s: %w", exe, err)
	}
	return cmd.Process.Release()
}

// fileVersion is the file version in a build's resources, as one number
// to compare: 0 when it has none or can't be looked at
// (internal/update's exeVersion).
func fileVersion(path string) uint64 {
	n, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || n == 0 {
		return 0
	}
	buf := make([]byte, n)
	if err := windows.GetFileVersionInfo(path, 0, n, unsafe.Pointer(&buf[0])); err != nil {
		return 0
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var size uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &size); err != nil || fixed == nil || size == 0 {
		return 0
	}
	return uint64(fixed.FileVersionMS)<<32 | uint64(fixed.FileVersionLS)
}

// versionText spells a file version as Windows does: 1.2.3.4.
func versionText(v uint64) string {
	return fmt.Sprintf("%d.%d.%d.%d", v>>48, (v>>32)&0xffff, (v>>16)&0xffff, v&0xffff)
}

// messageBox shows Windows' own box, over everything, and answers the
// button pressed.
func messageBox(title, text string, flags uint32) int {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	r, _, _ := procMessageBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), uintptr(flags|mbTopmost))
	return int(r)
}

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	procMessageBox        = user32.NewProc("MessageBoxW")
	procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
)
