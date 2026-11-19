package archbase

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	testPackageName  = "wazuh-agent-4.14.9-1-x86_64.pkg.tar.zst"
	testPackageBytes = "pinned wazuh package bytes"
	wazuhCommit      = "bed556db3f4f535cb3a585f3d9e7f35a6acf0f58"
)

func testPackageSum() string {
	sum := sha256.Sum256([]byte(testPackageBytes))
	return hex.EncodeToString(sum[:])
}

type builderRun struct {
	dir      string
	output   string
	err      error
	manifest string
	calls    []string
}

func (r builderRun) called(prefix string) int {
	return slices.IndexFunc(r.calls, func(call string) bool { return strings.HasPrefix(call, prefix) })
}

func (r builderRun) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(r.dir, "work", "rootfs", path))
	return err == nil
}

// runBuilder runs build.sh's real orchestration, package selection, Wazuh
// installation logic, overlay copy and manifest writer, replacing privileged
// and image-building operations with stubs. env overrides the defaults of a
// native build with a valid signed Wazuh package; a value of "" unsets a
// variable, and setup may adjust the input files.
func runBuilder(t *testing.T, env map[string]string, setup func(dir string)) builderRun {
	t.Helper()
	source, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	definitions := strings.TrimSuffix(string(source), "\nmain \"$@\"\n")
	if definitions == string(source) {
		t.Fatal("cannot isolate builder entrypoint")
	}
	// Pin the fixture package instead of the released one.
	pin := regexp.MustCompile(`(?m)^WAZUH_PACKAGE_PINNED_SHA256="[0-9a-f]{64}"$`)
	if len(pin.FindAllString(definitions, -1)) != 1 {
		t.Fatal("build.sh must pin exactly one wazuh-agent SHA-256")
	}
	definitions = pin.ReplaceAllString(definitions, `WAZUH_PACKAGE_PINNED_SHA256="`+testPackageSum()+`"`)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// An allowlisted PATH prevents an unstubbed mount, pacstrap, etc. from
	// ever reaching a real host command.
	for _, name := range []string{"dirname", "install", "rm", "mktemp", "cat", "date", "basename", "ln", "sha256sum", "rsync"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	inputs := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(inputs, testPackageName)
	if err := os.WriteFile(pkg, []byte(testPackageBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(inputs)
	}
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	harness := definitions + `
SCRIPT_DIR=` + scriptDir + `
: "${TEST_VERSION:=}" "${TEST_COMPARISON:=}" "${TEST_INSTALL:=}" "${TEST_WAZUH_VERSION:=}" "${TEST_SOURCE_COMMIT:=}"
call() { printf '%s\n' "$*" >>"${OUTPUT_DIR}/calls"; }
require_root() { :; }
require_commands() { :; }
build_kernel() { call build_kernel; : >"${VMLINUX_OUTPUT}"; }
install_kernel_modules() { :; }
install_pi() { :; }
install_agent_browser() { :; }
bootstrap_lazyvim() { :; }
configure_rootfs() { :; }
detach_rootfs() { :; }
truncate() { : >"${@: -1}"; }
mkfs.ext4() { :; }
losetup() { echo /dev/fake-loop; }
tune2fs() { :; }
mount() { :; }
install() {
  if [[ "$1" != -o ]]; then command install "$@"; fi
}
pacstrap() {
  install -d "${ROOTFS_MOUNT_DIR}/root"
  printf '%s\n' "$@" > "${OUTPUT_DIR}/pacstrap-args"
}
rsync() {
  call rsync "$@"
  # The real copy, minus the ownership change an unprivileged test cannot make.
  local args=() arg
  for arg in "$@"; do [[ "${arg}" == --chown=* ]] || args+=("${arg}"); done
  command rsync "${args[@]}"
}
arch-chroot() {
  [[ "$1" == "${ROOTFS_MOUNT_DIR}" ]] || return 90
  shift
  call "chroot $*"
  local stage="${ROOTFS_MOUNT_DIR}/root/srv-wazuh"
  case "$*" in
    'pacman -Q tailscale')
      [[ "${SRV_AMP_ORB}" != 1 && -n "${TEST_VERSION}" ]] || return 91
      printf 'tailscale %s\n' "${TEST_VERSION}"
      ;;
    "vercmp ${TEST_VERSION} 1.104.4")
      [[ "${TEST_COMPARISON}" != error ]] || return 92
      printf '%s\n' "${TEST_COMPARISON}"
      ;;
    'pacman --config /root/srv-wazuh/pacman.conf --noconfirm -U /root/srv-wazuh/'*)
      local command="$*"
      local staged="${ROOTFS_MOUNT_DIR}${command##* }"
      [[ "$(cat "${staged}")" == "$(cat "${WAZUH_PACKAGE}")" ]] || return 95
      # pacman's tmpfiles hook must see the package's entry masked.
      [[ -L "${ROOTFS_MOUNT_DIR}/etc/tmpfiles.d/wazuh-agent.conf" && "${ROOTFS_MOUNT_DIR}/etc/tmpfiles.d/wazuh-agent.conf" -ef /dev/null ]] || return 97
      cat "${stage}/pacman.conf" >"${OUTPUT_DIR}/wazuh-pacman.conf"
      if [[ "${TEST_INSTALL}" == reject ]]; then echo "error: failed to commit transaction (conflicting files)" >&2; return 96; fi
      install -d "${ROOTFS_MOUNT_DIR}/usr/share/wazuh-agent"
      printf '%s\n' "${TEST_SOURCE_COMMIT}" >"${ROOTFS_MOUNT_DIR}/usr/share/wazuh-agent/source-commit"
      if [[ "${TEST_INSTALL}" == enabled ]]; then
        install -d "${ROOTFS_MOUNT_DIR}/etc/systemd/system/multi-user.target.wants"
        : >"${ROOTFS_MOUNT_DIR}/etc/systemd/system/multi-user.target.wants/wazuh-agent.service"
      fi
      ;;
    'pacman -Q wazuh-agent') printf 'wazuh-agent %s\n' "${TEST_WAZUH_VERSION}" ;;
    '/usr/local/lib/srv/wazuh-owners.sh') ;;
    '/bin/bash /root/prepare-amp.'*) return 0 ;;
    *) echo "unexpected chroot command: $*" >&2; return 93 ;;
  esac
}
main
`
	script := filepath.Join(dir, "build-test.sh")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"PATH":               bin,
		"OUTPUT_DIR":         dir,
		"SRV_AMP_ORB":        "0",
		"TEST_VERSION":       "1.106.0-1",
		"TEST_COMPARISON":    "1",
		"WAZUH_PACKAGE":      pkg,
		"TEST_WAZUH_VERSION": "4.14.9-1",
		"TEST_SOURCE_COMMIT": wazuhCommit,
	}
	for k, v := range env {
		vars[k] = v
	}
	cmd := exec.Command("/bin/bash", script)
	for k, v := range vars {
		if v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	output, runErr := cmd.CombinedOutput()
	run := builderRun{dir: dir, output: string(output), err: runErr}
	if calls, err := os.ReadFile(filepath.Join(dir, "calls")); err == nil {
		run.calls = strings.Split(strings.TrimSpace(string(calls)), "\n")
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.txt"))
	switch {
	case err == nil:
		run.manifest = string(manifest)
	case !os.IsNotExist(err):
		t.Fatal(err)
	case runErr == nil:
		t.Fatalf("successful build wrote no manifest\n%s", output)
	}
	return run
}

func TestTailscaleImageFloor(t *testing.T) {
	for _, tc := range []struct {
		name, version, comparison string
		orb, wantFailure          bool
	}{
		{name: "below", version: "1.104.3-1", comparison: "-1", wantFailure: true},
		{name: "equal", version: "1.104.4", comparison: "0"},
		// vercmp ignores pkgrel when only one operand includes it.
		{name: "arch package release", version: "1.104.4-1", comparison: "0"},
		{name: "above", version: "1.106.0-1", comparison: "1"},
		{name: "missing package", wantFailure: true},
		{name: "comparison failure", version: "1.104.4-1", comparison: "error", wantFailure: true},
		{name: "empty comparison", version: "1.104.4-1", wantFailure: true},
		{name: "orb without tailscale", orb: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orb := "0"
			if tc.orb {
				orb = "1"
			}
			run := runBuilder(t, map[string]string{"SRV_AMP_ORB": orb, "TEST_VERSION": tc.version, "TEST_COMPARISON": tc.comparison}, nil)
			if (run.err != nil) != tc.wantFailure {
				t.Fatalf("builder error = %v, want failure %v\n%s", run.err, tc.wantFailure, run.output)
			}
			if tc.wantFailure {
				if run.manifest != "" {
					t.Fatalf("failed build published a manifest: %s", run.manifest)
				}
				if tc.name == "below" && !strings.Contains(run.output, "1.104.4") {
					t.Fatalf("missing minimum-version diagnostic: %s", run.output)
				}
				return
			}
			args, err := os.ReadFile(filepath.Join(run.dir, "pacstrap-args"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(args), "\ntailscale\n") == tc.orb {
				t.Fatalf("incorrect native Tailscale package selection (orb=%v): %s", tc.orb, args)
			}
			if tc.orb {
				if strings.Contains(run.manifest, "tailscale") {
					t.Fatalf("orb manifest must omit native Tailscale: %s", run.manifest)
				}
				if !strings.Contains(run.manifest, "\nsrv_amp_orb=1\n") {
					t.Fatalf("missing orb manifest flag: %s", run.manifest)
				}
			} else if !strings.Contains(run.manifest, "\ntailscale_version="+tc.version+"\n") {
				t.Fatalf("manifest must record resolved package version %q: %s", tc.version, run.manifest)
			}
		})
	}
}

func TestWazuhAgentInstall(t *testing.T) {
	run := runBuilder(t, nil, nil)
	if run.err != nil {
		t.Fatalf("builder failed: %v\n%s", run.err, run.output)
	}
	for _, line := range []string{
		"wazuh_agent_version=4.14.9-1",
		"wazuh_package_sha256=" + testPackageSum(),
		"wazuh_source_commit=" + wazuhCommit,
	} {
		if !strings.Contains(run.manifest, "\n"+line+"\n") {
			t.Errorf("manifest lacks %s:\n%s", line, run.manifest)
		}
	}
	// The agent installs after pacstrap and the Tailscale floor, and before
	// the overlay replaces its config.
	order := []string{
		"chroot pacman -Q tailscale",
		"chroot pacman --config /root/srv-wazuh/pacman.conf --noconfirm -U /root/srv-wazuh/" + testPackageName,
		"chroot pacman -Q wazuh-agent",
		"rsync ",
		"chroot /usr/local/lib/srv/wazuh-owners.sh",
	}
	last := -1
	for _, step := range order {
		i := run.called(step)
		if i <= last {
			t.Fatalf("%q at %d, want after step %d:\n%s", step, i, last, strings.Join(run.calls, "\n"))
		}
		last = i
	}
	conf, err := os.ReadFile(filepath.Join(run.dir, "wazuh-pacman.conf"))
	if err != nil {
		t.Fatal(err)
	}
	// The package is pinned by hash, not signed; repositories stay out of it.
	if !strings.Contains(string(conf), "\nLocalFileSigLevel = Optional\n") || strings.Contains(string(conf), "Include") || strings.Contains(string(conf), "Never") {
		t.Errorf("install pacman.conf:\n%s", conf)
	}
	if run.called("chroot pacman-key") >= 0 || run.called("chroot gpg") >= 0 {
		t.Errorf("build manages pacman keys:\n%s", strings.Join(run.calls, "\n"))
	}
	if run.exists("root/srv-wazuh") {
		t.Error("package staging directory left in the rootfs")
	}
	for _, path := range []string{
		"var/ossec/etc/ossec.conf",
		"usr/local/lib/srv/wazuh-enroll.sh",
		"etc/systemd/system/srv-wazuh-enroll.service",
		"etc/systemd/system/wazuh-agent.service.d/10-srv-require-enrollment.conf",
		"etc/tmpfiles.d/srv-wazuh.conf",
		"etc/tmpfiles.d/wazuh-agent.conf",
		"usr/local/lib/srv/wazuh-owners.sh",
		"etc/systemd/system/srv-wazuh-owners.service",
	} {
		if !run.exists(path) {
			t.Errorf("native rootfs lacks %s", path)
		}
	}
	args, err := os.ReadFile(filepath.Join(run.dir, "pacstrap-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "\nlibelf\n") {
		t.Errorf("pacstrap must provide wazuh-agent's libelf dependency: %s", args)
	}
}

func TestWazuhAgentInstallRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		env   map[string]string
		setup func(dir string)
		// early failures stop before the kernel build.
		early bool
	}{
		{name: "default path", want: "native guest images need WAZUH_PACKAGE: /srv/artifacts/wazuh-agent/4.14.9-1/wazuh-agent-4.14.9-1-x86_64.pkg.tar.zst does not exist", env: map[string]string{"WAZUH_PACKAGE": ""}, early: true},
		{name: "package missing", want: "native guest images need WAZUH_PACKAGE", env: map[string]string{"WAZUH_PACKAGE": "/nonexistent/" + testPackageName}, early: true},
		{name: "unpinned package", want: "not the pinned " + testPackageSum(), setup: func(dir string) {
			os.WriteFile(filepath.Join(dir, testPackageName), []byte("rebuilt package bytes"), 0o644)
		}, early: true},
		{name: "install fails", want: "failed to commit transaction", env: map[string]string{"TEST_INSTALL": "reject"}},
		{name: "older version", want: "is not 4.14.9", env: map[string]string{"TEST_WAZUH_VERSION": "4.14.8-1"}},
		{name: "version with shared prefix", want: "is not 4.14.9", env: map[string]string{"TEST_WAZUH_VERSION": "4.14.90-1"}},
		{name: "other source commit", want: "was not built from bed556db3f4f535cb3a585f3d9e7f35a6acf0f58", env: map[string]string{"TEST_SOURCE_COMMIT": "baad447a9fbed34f9dd3d23d5c916247da005ac6"}},
		{name: "package enables the agent", want: "must not be enabled", env: map[string]string{"TEST_INSTALL": "enabled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runBuilder(t, tc.env, tc.setup)
			if run.err == nil {
				t.Fatalf("builder accepted the package:\n%s", run.output)
			}
			if run.manifest != "" {
				t.Fatalf("failed build published a manifest: %s", run.manifest)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Fatalf("missing diagnostic %q:\n%s", tc.want, run.output)
			}
			if tc.early != (run.called("build_kernel") < 0) {
				t.Fatalf("early failure = %v, calls:\n%s\n%s", run.called("build_kernel") < 0, strings.Join(run.calls, "\n"), run.output)
			}
		})
	}
}

func TestImagesWithoutWazuh(t *testing.T) {
	noInputs := map[string]string{"WAZUH_PACKAGE": ""}
	for _, tc := range []struct {
		name, orb, agent, manifestLine string
	}{
		// Orb images omit the agent whatever SRV_WAZUH_AGENT says.
		{name: "orb", orb: "1", manifestLine: "srv_amp_orb=1"},
		{name: "orb with agent requested", orb: "1", agent: "1", manifestLine: "srv_amp_orb=1"},
		{name: "native opt-out", orb: "0", agent: "0", manifestLine: "wazuh_agent=disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"SRV_AMP_ORB": tc.orb, "SRV_WAZUH_AGENT": tc.agent}
			for k, v := range noInputs {
				env[k] = v
			}
			// Builds without the agent need no Wazuh inputs at all.
			run := runBuilder(t, env, nil)
			if run.err != nil {
				t.Fatalf("builder failed: %v\n%s", run.err, run.output)
			}
			if !strings.Contains(run.manifest, "\n"+tc.manifestLine+"\n") {
				t.Errorf("manifest lacks %s:\n%s", tc.manifestLine, run.manifest)
			}
			if tc.orb == "1" && strings.Contains(run.manifest, "wazuh") {
				t.Errorf("orb manifest mentions Wazuh:\n%s", run.manifest)
			}
			if strings.Contains(run.manifest, "wazuh_agent_version") || strings.Contains(run.manifest, "wazuh_package_sha256") {
				t.Errorf("manifest claims an installed agent:\n%s", run.manifest)
			}
			for _, call := range run.calls {
				if strings.Contains(call, "wazuh") && !strings.HasPrefix(call, "rsync ") || strings.Contains(call, "pacman-key") {
					t.Errorf("build ran %q", call)
				}
			}
			for _, path := range []string{
				"var/ossec",
				"usr/local/lib/srv/wazuh-enroll.sh",
				"etc/systemd/system/srv-wazuh-enroll.service",
				"etc/systemd/system/wazuh-agent.service.d",
				"etc/tmpfiles.d/srv-wazuh.conf",
				"etc/tmpfiles.d/wazuh-agent.conf",
				"usr/local/lib/srv/wazuh-owners.sh",
				"etc/systemd/system/srv-wazuh-owners.service",
			} {
				if run.exists(path) {
					t.Errorf("rootfs contains %s", path)
				}
			}
			if !run.exists("usr/local/lib/srv/bootstrap.sh") {
				t.Error("rootfs lost the rest of the overlay")
			}
			args, err := os.ReadFile(filepath.Join(run.dir, "pacstrap-args"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(args), "\nlibelf\n") {
				t.Errorf("pacstrap installs Wazuh dependencies: %s", args)
			}
			// The opt-out drops only the agent: native images keep the Tailscale floor.
			if tailscale := strings.Contains(string(args), "\ntailscale\n"); tailscale != (tc.orb == "0") {
				t.Errorf("tailscale selected = %v for orb=%s: %s", tailscale, tc.orb, args)
			}
			if tc.orb == "0" && !strings.Contains(run.manifest, "\ntailscale_version=") {
				t.Errorf("native opt-out skipped the Tailscale floor:\n%s", run.manifest)
			}
		})
	}
}

func TestWazuhAgentSwitch(t *testing.T) {
	noInputs := map[string]string{"WAZUH_PACKAGE": ""}
	for _, tc := range []struct{ name, agent, want string }{
		// The default and an explicit 1 both require the signed package.
		{name: "default", want: "native guest images need WAZUH_PACKAGE"},
		{name: "explicit on", agent: "1", want: "native guest images need WAZUH_PACKAGE"},
		// A misspelt opt-out must not silently change what ships.
		{name: "no", agent: "no", want: "SRV_WAZUH_AGENT must be 0 or 1"},
		{name: "false", agent: "false", want: "SRV_WAZUH_AGENT must be 0 or 1"},
		{name: "00", agent: "00", want: "SRV_WAZUH_AGENT must be 0 or 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"SRV_WAZUH_AGENT": tc.agent}
			for k, v := range noInputs {
				env[k] = v
			}
			run := runBuilder(t, env, nil)
			if run.err == nil || !strings.Contains(run.output, tc.want) {
				t.Fatalf("builder error = %v, want diagnostic %q:\n%s", run.err, tc.want, run.output)
			}
			if run.called("build_kernel") >= 0 {
				t.Fatalf("failed after starting the kernel build:\n%s", strings.Join(run.calls, "\n"))
			}
		})
	}
	// An explicit 1 with valid inputs installs the agent like the default.
	run := runBuilder(t, map[string]string{"SRV_WAZUH_AGENT": "1"}, nil)
	if run.err != nil || !strings.Contains(run.manifest, "\nwazuh_source_commit="+wazuhCommit+"\n") {
		t.Fatalf("explicit SRV_WAZUH_AGENT=1: %v\n%s\n%s", run.err, run.manifest, run.output)
	}
}

// The image must be fully unmounted before the manifest hashes it: a busy
// mount fails the build instead of being unmounted lazily, and the guest
// keyring's gpg agents are stopped first.
func TestDetachRootfsRefusesLazyUnmount(t *testing.T) {
	source, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	definitions := strings.TrimSuffix(string(source), "\nmain \"$@\"\n")
	for _, tc := range []struct {
		name, busy, mode, want string
		ok                     bool
	}{
		{name: "unmounts", ok: true, want: "gpgconf --homedir MNT/etc/pacman.d/gnupg --kill all\numount -R MNT\nlosetup -d /dev/fake-loop\n"},
		{name: "busy fails", busy: "1", want: "gpgconf --homedir MNT/etc/pacman.d/gnupg --kill all\numount -R MNT\n"},
		{name: "busy cleanup is lazy", busy: "1", mode: "lazy", ok: true, want: "gpgconf --homedir MNT/etc/pacman.d/gnupg --kill all\numount -R MNT\numount -Rl MNT\nlosetup -d /dev/fake-loop\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mnt := filepath.Join(dir, "mnt")
			if err := os.MkdirAll(filepath.Join(mnt, "etc/pacman.d/gnupg"), 0o700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "calls")
			harness := definitions + `
ROOTFS_MOUNT_DIR=` + mnt + `
LOOP_DEVICE=/dev/fake-loop
c() { printf '%s\n' "$*" >>` + log + `; }
mountpoint() { return 0; }
gpgconf() { c gpgconf "$@"; }
umount() { c umount "$@"; [[ "$1" == -Rl || -z "` + tc.busy + `" ]]; }
losetup() { c losetup "$@"; }
detach_rootfs ` + tc.mode + `
`
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			dirname, err := exec.LookPath("dirname")
			if err != nil {
				t.Skip(err)
			}
			if err := os.Symlink(dirname, filepath.Join(bin, "dirname")); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "detach-test.sh")
			if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script)
			cmd.Env = []string{"PATH=" + bin}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("err %v: %s", err, out)
			}
			calls, _ := os.ReadFile(log)
			if got := strings.ReplaceAll(string(calls), mnt, "MNT"); got != tc.want {
				t.Fatalf("calls:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
