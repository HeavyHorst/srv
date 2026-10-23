#!/usr/bin/env bash
# Run only inside a guest rootfs/chroot, never on the build host.
set -euo pipefail

[[ $(id -u) == 0 ]] || { echo 'guest preparation requires root' >&2; exit 1; }
[[ $(uname -m) == x86_64 ]] || { echo 'the pinned Amp Desktop bundle requires x86_64' >&2; exit 1; }
# shellcheck source=/dev/null
source /etc/os-release
case "$ID" in
	arch)
		pacman -S --needed --noconfirm bash coreutils util-linux ca-certificates python \
			glibc git nodejs tmux curl sudo ripgrep github-cli openssh \
			xorg-xwayland dbus gsettings-desktop-schemas xsettingsd wlr-randr \
			pulseaudio ffmpeg ttf-dejavu foot swaybg mesa libglvnd libinput \
			seatd libxcb xcb-util-wm libxkbcommon libxml2-legacy gtk3 librsvg patchelf
		;;
	debian)
		export DEBIAN_FRONTEND=noninteractive
		apt-get update
		apt-get install -y --no-install-recommends bash coreutils util-linux python3 \
			ca-certificates libc6 git nodejs tmux curl sudo ripgrep gh openssh-client \
			xwayland dbus-x11 gsettings-desktop-schemas libglib2.0-bin xsettingsd \
			wlr-randr pulseaudio ffmpeg fonts-dejavu-core foot swaybg libegl1 \
			libgles2 libgbm1 libinput10 libseat1 libxcb-composite0 libxcb-ewmh2 \
			libxcb-icccm4 libxcb-res0 libxcb-xfixes0 libxkbcommon0 libxml2 \
			libgtk-3-0t64 librsvg2-2 patchelf
		;;
	*) echo "unsupported guest distribution: $ID" >&2; exit 1 ;;
esac
node -e 'if (Number(process.versions.node.split(".")[0]) < 18) process.exit(1)'

# Copy only the Desktop bundle, not the upstream image's users or credentials.
# linux/amd64 manifest sha256:df8960d9dc5bf49e065af7de3e05928a1e9a4be3a60e42ef9e519c47df574abe
# Layer 18 is COPY /opt/amp-wayland in the official Amp image. Pin its digest so
# upstream tag changes cannot silently replace executable build inputs.
desktop_sha=d715df8263b45206b11ae9a77882fe540a0fb135feeeb01656e947bd293ed9a8
work=$(mktemp -d /tmp/amp-prepare.XXXXXX)
trap 'rm -rf "$work"' EXIT
curl -fsSL --retry 3 \
	"https://us-central1-docker.pkg.dev/v2/amp-prod-0/amp-orb/amp-orb/blobs/sha256:$desktop_sha" \
	-o "$work/desktop.tar.gz"
printf '%s  %s\n' "$desktop_sha" "$work/desktop.tar.gz" | sha256sum -c -
tar -xzf "$work/desktop.tar.gz" -C / --no-same-owner opt/amp-wayland
install -d -m 0755 /etc/profile.d
cat > /etc/amp-orb.env <<'EOF'
PATH=/opt/amp-wayland/bin:/opt/amp-wayland/libexec:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EOF
cat > /etc/profile.d/amp-wayland.sh <<'EOF'
. /etc/amp-orb.env
export PATH
EOF
# Private RPATHs resolve the bundle's transitive dependencies without leaking an
# old libwayland into distro applications (e.g. current Arch foot). Do not use a
# global LD_LIBRARY_PATH or register these libraries globally with ldconfig.
# shellcheck source=/dev/null
source /etc/amp-orb.env
export PATH
for binary in /opt/amp-wayland/bin/labwc /opt/amp-wayland/bin/labnag \
	/opt/amp-wayland/bin/wayland-scanner /opt/amp-wayland/bin/di-edid-decode \
	/opt/amp-wayland/libexec/waymote-streamd; do
	patchelf --force-rpath --set-rpath /opt/amp-wayland/lib/x86_64-linux-gnu:/opt/amp-wayland/lib "$binary"
	dependencies=$(ldd "$binary")
	if [[ "$dependencies" == *'not found'* ]]; then
		printf 'missing Desktop dependency for %s:\n%s\n' "$binary" "$dependencies" >&2
		exit 1
	fi
done
labwc --version
waymote-gateway -version
waymote-streamd --version
# Distro clients must still start with their own (possibly newer) libraries.
foot --version
Xwayland -version
printf '%s\n' "$desktop_sha" > /opt/amp-wayland/srv-layer.sha256

getent group user >/dev/null || groupadd user
if ! id user >/dev/null 2>&1; then
	useradd --create-home --home-dir /home/user --gid user --shell /bin/bash user
fi
[[ $(getent passwd user | cut -d: -f6) == /home/user && $(id -gn user) == user ]] || {
	echo 'user must have home /home/user and primary group user' >&2
	exit 1
}
install -d -m 0755 -o user -g user /home/user /home/user/.amp
install -d -m 0755 /etc/sudoers.d /usr/local/bin
printf 'user ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/amp-user
chmod 0440 /etc/sudoers.d/amp-user
visudo -cf /etc/sudoers.d/amp-user

# Fetch separately so a failed download cannot be hidden by a successful shell.
installer="$work/install.sh"
curl -fsSL https://ampcode.com/install.sh -o "$installer"
# Install as the runtime user: both the binary and its parent must be writable
# for Amp's atomic self-updates. Do not inherit build-host credentials.
runuser -u user -- env -i HOME=/home/user USER=user LOGNAME=user \
	PATH=/usr/local/bin:/usr/bin:/bin AMP_HOME=/home/user/.amp \
	bash < "$installer"
test -x /home/user/.amp/bin/amp
ln -sfn /home/user/.amp/bin/amp /usr/local/bin/amp
