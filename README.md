# rawim

Convert a container image into a bootable raw disk image (GPT, systemd-boot, ext4 root).
Supports Rocky Linux and Ubuntu container images.

## Getting Started

1. Install prerequisites on the build host. On Ubuntu this is the following `apt` packages (tools shown in brackets, if different):
    - `go`
    - `podman`
    - `parted` (`partprobe`)
    - `dosfstools` (`mkfs.vfat`)
    - `e2fsprogs` (`mkfs.ext4`, `e2label`)
    - `dosfstools` (`fatlabel`)
    - `util-linux` (`losetup`, `blockdev`)
    - `coreutils` (`truncate`, `numfmt`)
    - `libcap2-bin` (`setcap`)

    ```bash
    sudo apt-get install -y podman parted dosfstools e2fsprogs util-linux libcap2-bin
    ```
    
2. Clone this repository and build the binary:
    ```bash
    go build -o rawim .
    ```
  
    Or install directly to ~/go/bin (make sure ~/go/bin is on your PATH):
    
    ```bash
    go install .
    ```

3. Build a container image which includes the [required packages](./README.md#required-packages--the-containerfile).
    E.g. using the example RockyLinux 9 container file provided in this repository:
    
    ```shell
    podman build -f containerfiles/rockylinux9 -t localhost/rocky9sysimg:latest
    ```

    > **Note:** The tool mounts loop devices and runs privileged containers, so it must be run as root (or with sudo). Therefore it is usually more convenient to run
    the container build as root too.

    The section below provides some hints on how to make iterating on builds fast.

4. Convert it into a bootable raw image:

    ```shell
    sudo ./rawim localhost/rocky9sysimg:latest
    ```


## Usage

```
rawim [options] <container-image>

Options:
  -output string
        Output raw image path (default: <image-name>.raw)
  -root-extra string
        Extra space for root partition, numfmt --from=auto syntax (default "200M")
  -esp string
        ESP partition size, format as for -root-extra syntax (default "200M")
  -root-mount string
        Temporary mount point for root partition (default: /mnt/<image-name>)
  -capfiles-user string
        Additional capabilities to set on files - these cannot be represented in a container filesystem so must be added when building the system image. This is a space-separated string where each "word" is in the format `absolute_path:capability` where the capability is a single clause of the [capability set test format](https://man7.org/linux/man-pages/man7/cap_text_formats.7.html)
        as used for e.g. `setcap`.E.g. '/usr/bin/foo:cap_net_raw=p /usr/bin/bar:cap_setgid=ep' modifies a single capability name for each of two files. It is not an error for the specified file to not exist. In additional to capabilities set this way a default set of capabilities is applied, intended to match capabilities on upstream cloud images.
```

### Examples

```bash
# Basic – output defaults to myimage.raw
sudo rawim docker.io/myrepo/myimage:latest

# Custom output path and larger root partition
sudo rawim -output /var/images/server.raw -root-extra 1G docker.io/myrepo/myimage:latest

# Extra capability on top of the built-in defaults
sudo rawim -capfiles-user '/usr/sbin/tcpdump:cap_net_raw=p' docker.io/myrepo/myimage:latest

# IEC sizes (powers of two)
sudo rawim -esp 256Mi -root-extra 512Mi docker.io/myrepo/myimage:latest
```

## How it works

The key idea is that the container image is the root filesystem, but anything that needs to touch the
disk (initramfs, bootloader, fstab) is done **inside a running privileged container** of that same image,
so the distro's own tools (`dracut`, `bootctl`, `systemctl`, `dnf`/`dpkg`) are used rather than
requiring them on the build host. Only the final export is done from the host. Steps:

1. **Size the disk.** Image size = 1 MiB (GPT headers/alignment) + ESP size + the container image size
   (from `podman image inspect`) + `-root-extra` headroom, rounded up to 512 bytes. Sizes use
   `numfmt --from=auto` syntax (`200M` is decimal, `200Mi` is binary).
2. **Create the raw file** with `truncate` (sparse) and attach it with `losetup -fP`.
3. **Partition and format:** GPT with p1 = FAT32 ESP (label `ESP`, flagged `esp`), starting at 1 MiB, and
   p2 = ext4 root (label `rootfs`) filling the rest. Filesystems are referenced by **label**, so the
   device name does not matter at boot time.
4. **Start a privileged container** from the image (`sleep infinity`) with the loop device passed through.
   The distro is detected from `/etc/os-release` (`ID`: `rocky` or `ubuntu`; anything else is rejected).
5. **Check the image has the required packages** (see below) and find the kernel: the highest version in
   `/lib/modules`. The kernel file is `/lib/modules/<ver>/vmlinuz` on Rocky and `/boot/vmlinuz-<ver>` on Ubuntu.
6. **Regenerate the initramfs in the container** with `dracut`, forcing in the virtio block/PCI/SCSI
   drivers and ext4/vfat support, so the image boots under KVM/QEMU and clouds as well as on bare metal.
7. **Install the bootloader in the container:** mount the ESP (partition 1 of the loop device) at
   `/boot/efi` and run `bootctl install --esp-path=/boot/efi`. Then write `loader.conf` and a single
   boot entry (`root=LABEL=rootfs ro`, console on both `tty0` and `ttyS0,115200n8`), and copy the
   kernel and initramfs *onto the ESP* (systemd-boot can only read FAT, so they must live there).
8. **Write `/etc/fstab`** (by label) and enable `serial-getty@ttyS0` (non-fatal if this fails).
9. **Sanity check sizes:** `du -sbx /` in the container must fit in the root partition, otherwise fail
   early, before the slow export.
10. **Export the root filesystem:** mount the root partition on the host (`-root-mount`) and stream
    `podman export <container> | tar -xp` into it (excluding `boot/efi/*`, which is the ESP's contents
    and is already on its own partition). Because this is the *container's* filesystem after the steps
    above, it contains the fstab, initramfs and so on.
11. **Fix file capabilities:** Extended attributes are not
    preserved in a container filesystem, so they are re-applied with `setcap` on the mounted root.
    A per-distro default list (e.g. `ping`, `arping`, `newuidmap`/`newgidmap`) is applied to match upstream
    cloud images, plus anything given with `-capfiles-user`. Missing files are skipped. Finally the empty
    `/boot/efi` mount point is created, and everything is `sync`ed.
12. **Cleanup** always runs (even on failure): remove the container, detach the loop device, unmount the root.

## Required packages / the containerfile

The tool does not install anything into the image, it only checks. The container image must already
contain a kernel, systemd, systemd-boot and dracut, plus whatever you need for the system to be useful
once booted (networking, ssh, cloud-init, ...) with relevant services enabled via `systemctl enable`
(this works in a container build, as it just creates symlinks).

| Distro (`ID`) | Checked packages |
|---|---|
| `rocky` | `systemd`, `systemd-boot-unsigned`, `kernel`, `dracut` |
| `ubuntu` | `systemd`, `systemd-boot`, `linux-image-generic`, `dracut` |

See the examples in [containerfiles/](./containerfiles): `rockylinux9`, `ubuntu24` and `openhpc`
(Rocky plus OpenHPC packages).

### Hints for fast iteration

If image builds are going be repeated, e.g. during development,
package installs can be sped up by mounting a package cache from the
host into the build, so repeated builds reuse the already-downloaded
packages instead of fetching them again, e.g.:

```shell
# dnf (Rocky) – needs keepcache=1 in the containerfile
sudo mkdir -p /var/cache/rawim/dnf
sudo podman build -v /var/cache/rawim/dnf:/var/cache/dnf:Z \
    -f containerfiles/rockylinux9 -t localhost/rocky9sysimg:latest
```

By default `dnf` deletes downloaded packages after installing them,
which defeats this cache. To avoid this add the following in the
Containerfile before any `dnf` operations:

```shell
RUN echo 'keepcache=1' >> /etc/dnf/dnf.conf
```

`rawim` deletes any `keepcache=1` and `tsflags=nodocs` lines from
`/etc/dnf/dnf.conf` in the final image anyway to restore defaults, so
these build-time tweaks do not leak into it.

This approach has the additional benefit of not requiring the standard
`dnf install ... && dnf clean all` pattern to stop the cache being
preserved in intermediate layers. Therefore installs can be split into
many `RUN` lines to improve layer caching without bloating the image,
which in turn reduces the final disk size.

For `ubuntu` the same approach applies using `-v` mounts for
`/var/cache/apt` and `/var/lib/apt/lists`. The   official Ubuntu image
disables package caching through `/etc/apt/apt.conf.d/docker-clean`, so
delete that file in the containerfile and set
`Binary::apt::APT::Keep-Downloaded-Packages "true";`
in an apt config snippet. Note that `containerfiles/ubuntu24` as
provided uses the plain `apt-get clean` pattern and does not do this.
