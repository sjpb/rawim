# rawim

Converts a container image into a bootable raw disk image (GPT, systemd-boot, ext4 root).
Supports Rocky Linux and Ubuntu container images.

## Getting Started

1. Install prerequisites on the build host. On Ubuntu this is the following `apt` packages (tools shown in brackets, if different):
- `go`
- `podman`
- `parted`
- `dosfstools` (`mkfs.vfat`)
- `e2fsprogs` (`mkfs.ext4`, `e2label`)
- `dosfstools` (`fatlabel`)
- `parted` (`partprobe`)
- `util-linux` (`losetup`, `blockdev`)
- `coreutils` (`truncate`, `numfmt`)
- `libcap2-bin` (`setcap`)

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

Note the section below provides some hints on how to make iterating on builds fast.

4. Convert it into a bootable raw image:

   ```shell
   ./rawim.sh localhost/rocky9sysimg:latest
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

Environment variables (flags take precedence):
  ROOT_MOUNT      overrides -root-mount
  CAPFILES_USER   overrides -capfiles-user
```

### Examples

```bash
# Basic – output defaults to myimage.raw
sudo ctr2raw docker.io/myrepo/myimage:latest

# Custom output path and larger root partition
sudo ctr2raw -output /var/images/server.raw -root-extra 1G docker.io/myrepo/myimage:latest

# Extra capability on top of the built-in defaults
sudo ctr2raw -capfiles-user '/usr/sbin/tcpdump:cap_net_raw=p' docker.io/myrepo/myimage:latest

# IEC sizes (powers of two)
sudo ctr2raw -esp 256Mi -root-extra 512Mi docker.io/myrepo/myimage:latest
```

> **Note:** The tool mounts loop devices and runs privileged containers, so it must be run as root (or with sudo).

## Runtime dependencies

The following tools must be present on the **host** (not in the container):

| Tool | Package (Ubuntu) |
|------|-----------------|
| `podman` | `podman` |
| `parted` | `parted` |
| `mkfs.vfat` | `dosfstools` |
| `mkfs.ext4` | `e2fsprogs` |
| `e2label` | `e2fsprogs` |
| `fatlabel` | `dosfstools` |
| `partprobe` | `parted` |
| `losetup` | `util-linux` |
| `blockdev` | `util-linux` |
| `truncate` | `coreutils` |
| `setcap` | `libcap2-bin` |
| `numfmt` | `coreutils` |

Install them all at once:

```bash
sudo apt-get install -y podman parted dosfstools e2fsprogs util-linux libcap2-bin
```

The container image itself must contain `dracut`, `bootctl` (`systemd-boot`), and a kernel.
