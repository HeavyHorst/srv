#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT_DIR="${OUTPUT_DIR:-${SCRIPT_DIR}/out}"
WORK_DIR="${WORK_DIR:-${OUTPUT_DIR}/work}"
ROOTFS_MOUNT_DIR="${WORK_DIR}/rootfs"

ARCH="${ARCH:-x86_64}"
ROOTFS_SIZE="${ROOTFS_SIZE:-4G}"
ROOTFS_LABEL="${ROOTFS_LABEL:-srv-root}"
# Use the current 6.12 longterm series by default to avoid older-kernel
# toolchain friction on modern distros such as Arch with GCC 15.
KERNEL_VERSION="${KERNEL_VERSION:-6.12.79}"
FIRECRACKER_CONFIG_VERSION="${FIRECRACKER_CONFIG_VERSION:-6.1}"

KERNEL_TARBALL="${WORK_DIR}/linux-${KERNEL_VERSION}.tar.xz"
KERNEL_SOURCE_DIR="${WORK_DIR}/linux-${KERNEL_VERSION}"
FIRECRACKER_CONFIG_PATH="${WORK_DIR}/microvm-kernel-ci-${ARCH}-${FIRECRACKER_CONFIG_VERSION}.config"
KERNEL_TARBALL_URL="${KERNEL_TARBALL_URL:-https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz}"
FIRECRACKER_CONFIG_URL="${FIRECRACKER_CONFIG_URL:-https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-ci-${ARCH}-${FIRECRACKER_CONFIG_VERSION}.config}"

VMLINUX_OUTPUT="${OUTPUT_DIR}/vmlinux"
ROOTFS_OUTPUT="${OUTPUT_DIR}/rootfs-base.img"
MANIFEST_OUTPUT="${OUTPUT_DIR}/manifest.txt"
PACMAN_CONFIG_PATH="${WORK_DIR}/pacman.conf"
PACMAN_HOOKS_DIR="${WORK_DIR}/pacman-hooks"
KERNEL_RELEASE=""

ROOTFS_PACKAGES=(
	base
	ca-certificates
	curl
	docker
	docker-compose
	iproute2
	iptables-nft
	jq
	kmod
	tailscale
)

LOOP_DEVICE=""

require_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		echo "run this script as root so it can mount the rootfs and run pacstrap" >&2
		exit 1
	fi
}

require_commands() {
	local missing=()
	local cmd
	for cmd in \
		bc \
		bison \
		curl \
		depmod \
		flex \
		gcc \
		losetup \
		make \
		mkfs.ext4 \
		mount \
		mountpoint \
		nproc \
		pacstrap \
		perl \
		rsync \
		systemctl \
		tar \
		truncate \
		tune2fs \
		umount; do
		if ! command -v "${cmd}" >/dev/null 2>&1; then
			missing+=("${cmd}")
		fi
	done
	if [[ "${#missing[@]}" -gt 0 ]]; then
		echo "missing required commands: ${missing[*]}" >&2
		echo "on Arch, install arch-install-scripts, base-devel, bc, e2fsprogs, rsync, and curl" >&2
		exit 1
	fi
}

cleanup() {
	set +e
	detach_rootfs
}

detach_loop_device() {
	local attempt
	if [[ -z "${LOOP_DEVICE}" ]]; then
		return
	fi
	for attempt in {1..20}; do
		if losetup -d "${LOOP_DEVICE}" 2>/dev/null; then
			LOOP_DEVICE=""
			return
		fi
		if command -v udevadm >/dev/null 2>&1; then
			udevadm settle >/dev/null 2>&1 || true
		fi
		sleep 0.2
	done
	losetup -d "${LOOP_DEVICE}"
	LOOP_DEVICE=""
}

detach_rootfs() {
	if mountpoint -q "${ROOTFS_MOUNT_DIR}"; then
		umount -R "${ROOTFS_MOUNT_DIR}" || umount -Rl "${ROOTFS_MOUNT_DIR}"
	fi
	detach_loop_device
}

fetch() {
	local url="$1"
	local dest="$2"
	if [[ -f "${dest}" ]]; then
		return
	fi
	install -d "$(dirname -- "${dest}")"
	curl -fsSL "${url}" -o "${dest}.tmp"
	mv "${dest}.tmp" "${dest}"
}

kernel_jobs() {
	local cpu_jobs mem_kib mem_jobs
	if [[ -n "${KERNEL_JOBS:-}" ]]; then
		echo "${KERNEL_JOBS}"
		return
	fi

	cpu_jobs="$(nproc)"
	mem_kib="$(awk '/MemAvailable:/ { print $2; found=1; exit } /MemTotal:/ { print $2; exit }' /proc/meminfo)"
	# Keep kernel builds conservative: one job per ~2 GiB of available RAM.
	mem_jobs=$(( mem_kib / 2097152 ))
	if (( mem_jobs < 1 )); then
		mem_jobs=1
	fi
	if (( mem_jobs < cpu_jobs )); then
		echo "${mem_jobs}"
		return
	fi
	echo "${cpu_jobs}"
}

write_pacman_config() {
	rm -rf "${PACMAN_HOOKS_DIR}"
	install -d "${PACMAN_HOOKS_DIR}"
	# The guest boots our separately built vmlinux, so suppress mkinitcpio hooks
	# from the host environment during pacstrap.
	ln -sf /dev/null "${PACMAN_HOOKS_DIR}/90-mkinitcpio-install.hook"
	cat >"${PACMAN_CONFIG_PATH}" <<EOF
[options]
Architecture = auto
CheckSpace
SigLevel = Required DatabaseOptional
LocalFileSigLevel = Optional
ParallelDownloads = 5
HookDir = ${PACMAN_HOOKS_DIR}

[core]
Include = /etc/pacman.d/mirrorlist

[extra]
Include = /etc/pacman.d/mirrorlist

[multilib]
Include = /etc/pacman.d/mirrorlist
EOF
}

build_kernel() {
	local jobs kernel_config
	jobs="$(kernel_jobs)"
	kernel_config="${KERNEL_SOURCE_DIR}/.config"

	fetch "${KERNEL_TARBALL_URL}" "${KERNEL_TARBALL}"
	fetch "${FIRECRACKER_CONFIG_URL}" "${FIRECRACKER_CONFIG_PATH}"

	if [[ ! -d "${KERNEL_SOURCE_DIR}" ]]; then
		tar -xf "${KERNEL_TARBALL}" -C "${WORK_DIR}"
	fi

	make -C "${KERNEL_SOURCE_DIR}" mrproper
	cp "${FIRECRACKER_CONFIG_PATH}" "${kernel_config}"
	KCONFIG_CONFIG="${kernel_config}" "${KERNEL_SOURCE_DIR}/scripts/kconfig/merge_config.sh" -m \
		"${kernel_config}" \
		"${SCRIPT_DIR}/kernel-fragment.config"
	make -C "${KERNEL_SOURCE_DIR}" KCONFIG_CONFIG="${kernel_config}" olddefconfig
	KERNEL_RELEASE="$(make -s -C "${KERNEL_SOURCE_DIR}" KCONFIG_CONFIG="${kernel_config}" kernelrelease)"
	echo "building kernel with ${jobs} parallel job(s)"
	make -C "${KERNEL_SOURCE_DIR}" KCONFIG_CONFIG="${kernel_config}" -j"${jobs}" vmlinux modules

	install -m 0644 "${KERNEL_SOURCE_DIR}/vmlinux" "${VMLINUX_OUTPUT}"
}

install_kernel_modules() {
	local kernel_config modules_dir
	kernel_config="${KERNEL_SOURCE_DIR}/.config"
	if [[ -z "${KERNEL_RELEASE}" ]]; then
		echo "kernel release is unset; build_kernel must run before build_rootfs" >&2
		exit 1
	fi

	make -C "${KERNEL_SOURCE_DIR}" \
		KCONFIG_CONFIG="${kernel_config}" \
		INSTALL_MOD_PATH="${ROOTFS_MOUNT_DIR}" \
		modules_install
	depmod -b "${ROOTFS_MOUNT_DIR}" "${KERNEL_RELEASE}"
	modules_dir="${ROOTFS_MOUNT_DIR}/lib/modules/${KERNEL_RELEASE}"
	rm -f "${modules_dir}/build" "${modules_dir}/source"
}

configure_rootfs() {
	install -d "${ROOTFS_MOUNT_DIR}/var/lib/srv"
	chmod 0755 "${ROOTFS_MOUNT_DIR}/usr/local/lib/srv/bootstrap.sh"
	systemctl --root="${ROOTFS_MOUNT_DIR}" enable docker.service tailscaled.service srv-bootstrap.service >/dev/null
	truncate -s 0 "${ROOTFS_MOUNT_DIR}/etc/machine-id"
	rm -f "${ROOTFS_MOUNT_DIR}/var/lib/dbus/machine-id"
	rm -rf "${ROOTFS_MOUNT_DIR}/var/cache/pacman/pkg"
	install -d -m 0755 "${ROOTFS_MOUNT_DIR}/var/cache/pacman/pkg"
	rm -f "${ROOTFS_MOUNT_DIR}/etc/resolv.conf"
	ln -s /proc/net/pnp "${ROOTFS_MOUNT_DIR}/etc/resolv.conf"
}

build_rootfs() {
	rm -f "${ROOTFS_OUTPUT}"
	truncate -s "${ROOTFS_SIZE}" "${ROOTFS_OUTPUT}"
	mkfs.ext4 -F -L "${ROOTFS_LABEL}" "${ROOTFS_OUTPUT}"

	LOOP_DEVICE="$(losetup --show -fP "${ROOTFS_OUTPUT}")"
	tune2fs -c 0 -i 0 "${LOOP_DEVICE}" >/dev/null
	mount "${LOOP_DEVICE}" "${ROOTFS_MOUNT_DIR}"

	write_pacman_config
	pacstrap -C "${PACMAN_CONFIG_PATH}" -K -c "${ROOTFS_MOUNT_DIR}" "${ROOTFS_PACKAGES[@]}"
	install_kernel_modules

	rsync -a "${SCRIPT_DIR}/overlay/" "${ROOTFS_MOUNT_DIR}/"
	configure_rootfs
	detach_rootfs
}

write_manifest() {
	cat >"${MANIFEST_OUTPUT}" <<EOF
built_at=$(date --iso-8601=seconds)
arch=${ARCH}
kernel_version=${KERNEL_VERSION}
kernel_release=${KERNEL_RELEASE}
firecracker_config_url=${FIRECRACKER_CONFIG_URL}
rootfs_size=${ROOTFS_SIZE}
packages=$(IFS=,; echo "${ROOTFS_PACKAGES[*]}")
artifacts=$(basename -- "${VMLINUX_OUTPUT}"),$(basename -- "${ROOTFS_OUTPUT}")
EOF
}

main() {
	trap cleanup EXIT
	require_root
	require_commands
	install -d "${OUTPUT_DIR}" "${WORK_DIR}" "${ROOTFS_MOUNT_DIR}"
	build_kernel
	build_rootfs
	write_manifest
	echo "built ${VMLINUX_OUTPUT}"
	echo "built ${ROOTFS_OUTPUT}"
	echo "wrote ${MANIFEST_OUTPUT}"
}

main "$@"
