// rawim converts a container image into a bootable raw disk image.
//
// Usage:
//
//	rawim [options] <container-image>
//
// Example:
//
//	rawim -output myimage.raw -root-extra 200M -esp 200M docker.io/myrepo/myimage:latest
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// -----------------------------------------------------------------------------
// CLI / config
// -----------------------------------------------------------------------------

type config struct {
	containerImage string
	outputImage    string
	rootExtraSize  string
	espSize        string
	rootMount      string
	capfilesUser   string
}

func parseArgs() (config, error) {
	cfg := config{}

	fs := flag.NewFlagSet("rawim", flag.ContinueOnError)
	fs.StringVar(&cfg.outputImage, "output", "", "Output raw image path (default: <image-name>.raw)")
	fs.StringVar(&cfg.rootExtraSize, "root-extra", "200M", "Extra space for root partition (numfmt --from=auto syntax, e.g. 200M, 512Mi)")
	fs.StringVar(&cfg.espSize, "esp", "200M", "ESP partition size (same syntax)")
	fs.StringVar(&cfg.rootMount, "root-mount", "", "Mount point for root partition (default: /mnt/<image-name>)")
	fs.StringVar(&cfg.capfilesUser, "capfiles-user", "", "Extra file capabilities: space-separated 'absolute_path:capability' entries, e.g. '/usr/bin/foo:cap_net_raw=p /usr/bin/bar:cap_setgid=ep'")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: rawim [options] <container-image>\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		return cfg, err
	}

	// Positional: container image is required.
	if fs.NArg() != 1 {
		fs.Usage()
		return cfg, errors.New("exactly one positional argument required: <container-image>")
	}
	cfg.containerImage = fs.Arg(0)

	// Derive defaults that depend on the image name.
	_, name := splitImageName(cfg.containerImage)
	if cfg.outputImage == "" {
		cfg.outputImage = name + ".raw"
	}
	if cfg.rootMount == "" {
		cfg.rootMount = "/mnt/" + name
	}

	return cfg, nil
}

// splitImageName mirrors the bash TAG/IMAGE_NAME extraction.
//
//	"registry/repo/name:tag"       → tag="tag", name="name"
//	"localhost:5000/name"          → tag="",    name="name"
//	"registry/repo/name@sha256:x"  → tag="",    name="name"
func splitImageName(image string) (tag, name string) {
	// Only the last path component can carry a tag or digest; earlier
	// components may contain a registry port ("host:5000").
	name = image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "@"); i >= 0 { // drop digest
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
		name = name[:i]
	}
	return
}

// -----------------------------------------------------------------------------
// Shell helpers
// -----------------------------------------------------------------------------

// run executes a command, streaming stdout/stderr to our own stdout/stderr.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runOutput executes a command and returns trimmed stdout.
// Any stderr is appended to the returned error.
func runOutput(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return strings.TrimSpace(stdout.String()), err
}

// humanIEC formats a byte count like "numfmt --to=iec".
func humanIEC(n int64) string {
	out, err := runOutput("numfmt", "--to=iec", strconv.FormatInt(n, 10))
	if err != nil {
		return strconv.FormatInt(n, 10)
	}
	return out
}

// waitForDevices waits (briefly) for device nodes to appear after partprobe.
func waitForDevices(paths ...string) error {
	_ = run("udevadm", "settle")
	deadline := time.Now().Add(10 * time.Second)
	for _, p := range paths {
		for {
			if _, err := os.Stat(p); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for %s", p)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// podmanExec runs a command inside a running container (podman exec <ctr> ...).
func podmanExec(ctr string, args ...string) error {
	allArgs := append([]string{"exec", ctr}, args...)
	return run("podman", allArgs...)
}

// podmanExecOutput runs a command inside a container and returns stdout.
func podmanExecOutput(ctr string, args ...string) (string, error) {
	allArgs := append([]string{"exec", ctr}, args...)
	return runOutput("podman", allArgs...)
}

// podmanExecShell runs a shell snippet inside a container.
func podmanExecShell(ctr, script string) error {
	return podmanExec(ctr, "bash", "-c", script)
}

// -----------------------------------------------------------------------------
// numfmt wrapper
// -----------------------------------------------------------------------------

// parseSize converts a human-readable size (numfmt --from=auto syntax) to bytes.
func parseSize(s string) (int64, error) {
	out, err := runOutput("numfmt", "--from=auto", s)
	if err != nil {
		return 0, fmt.Errorf("numfmt failed for %q: %w", s, err)
	}
	return strconv.ParseInt(out, 10, 64)
}

// -----------------------------------------------------------------------------
// Main logic
// -----------------------------------------------------------------------------

func main() {
	log.SetFlags(0) // no timestamp prefix – matches bash script style
	log.SetPrefix("rawim: ")

	cfg, err := parseArgs()
	if err != nil {
		log.Fatal(err)
	}

	if os.Geteuid() != 0 {
		log.Fatal("must be run as root (needs loop devices, mounts and privileged containers)")
	}

	if err := buildImage(cfg); err != nil {
		log.Fatalf("failed: %v", err)
	}
}

func buildImage(cfg config) error {
	tag, name := splitImageName(cfg.containerImage)
	fmt.Printf("CTR_IMAGE: %s  TAG: %s  IMAGE_NAME: %s  OUTPUT_IMAGE: %s\n",
		cfg.containerImage, tag, name, cfg.outputImage)
	fmt.Printf("Will create %s from %s\n", cfg.outputImage, cfg.containerImage)

	// -------------------------------------------------------------------------
	// 1. Inspect container image size
	// -------------------------------------------------------------------------
	ctrImageSizeStr, err := runOutput("podman", "image", "inspect",
		"--format", "{{.Size}}", cfg.containerImage)
	if err != nil {
		return fmt.Errorf("podman image inspect: %w", err)
	}
	ctrImageBytes, err := strconv.ParseInt(ctrImageSizeStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing image size %q: %w", ctrImageSizeStr, err)
	}
	fmt.Printf("Container image is %d bytes\n", ctrImageBytes)

	// -------------------------------------------------------------------------
	// 2. Calculate raw image size
	// -------------------------------------------------------------------------
	rootExtraBytes, err := parseSize(cfg.rootExtraSize)
	if err != nil {
		return err
	}
	espBytes, err := parseSize(cfg.espSize)
	if err != nil {
		return err
	}
	reservedStartBytes := int64(1 * 1024 * 1024) // 1 MiB GPT headers
	reservedEndBytes := int64(2 * 1024 * 1024)   // ESP rounding up to a MiB + backup GPT
	requiredSize := reservedStartBytes + espBytes + ctrImageBytes + rootExtraBytes + reservedEndBytes
	imageSize := (requiredSize + 511) / 512 * 512 // round up to 512-byte boundary

	// -------------------------------------------------------------------------
	// 3. Create raw disk image
	// -------------------------------------------------------------------------
	humanSize, _ := runOutput("numfmt", "--to=si", strconv.FormatInt(imageSize, 10))
	fmt.Printf("Creating %s raw image...\n", humanSize)

	if err := os.Remove(cfg.outputImage); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing image: %w", err)
	}
	if err := run("truncate", "-s", strconv.FormatInt(imageSize, 10), cfg.outputImage); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	// -------------------------------------------------------------------------
	// 4. Set up loop device
	// -------------------------------------------------------------------------
	loopDev, err := runOutput("losetup", "-fP", "--show", cfg.outputImage)
	if err != nil {
		return fmt.Errorf("losetup: %w", err)
	}
	fmt.Printf("Using loop device: %s\n", loopDev)

	// Cleanup function – mirrors bash trap cleanup EXIT.
	var (
		ctr          string // set later; captured by closure
		rootMounted  bool   // true once we have mounted the root partition
		createdMount bool   // true if we created the mount point directory
		succeeded    bool
	)
	cleanup := func() {
		fmt.Println("Cleaning up...")
		if ctr != "" {
			_ = run("podman", "rm", "--force", "--time=1", ctr)
		}
		if rootMounted {
			_ = run("umount", cfg.rootMount)
		}
		if createdMount {
			_ = os.Remove(cfg.rootMount) // only removes it if empty
		}
		_ = run("losetup", "-d", loopDev)
		if !succeeded {
			_ = os.Remove(cfg.outputImage) // don't leave a partial image
		}
	}
	defer cleanup()

	// -------------------------------------------------------------------------
	// 5. Partition the disk
	// -------------------------------------------------------------------------
	fmt.Println("Partitioning disk...")
	espEndBytes := int64(1048576) + espBytes
	espEndMiB := (espEndBytes + 1048575) / 1048576 // round UP to nearest MiB
	espEndStr := fmt.Sprintf("%dMiB", espEndMiB)

	if err := run("parted", "-s", loopDev, "mklabel", "gpt"); err != nil {
		return fmt.Errorf("parted mklabel: %w", err)
	}
	if err := run("parted", "-s", loopDev, "mkpart", "primary", "fat32", "1MiB", espEndStr); err != nil {
		return fmt.Errorf("parted mkpart esp: %w", err)
	}
	if err := run("parted", "-s", loopDev, "set", "1", "esp", "on"); err != nil {
		return fmt.Errorf("parted set esp: %w", err)
	}
	if err := run("parted", "-s", loopDev, "mkpart", "primary", "ext4", espEndStr, "100%"); err != nil {
		return fmt.Errorf("parted mkpart root: %w", err)
	}

	if err := run("partprobe", loopDev); err != nil {
		return fmt.Errorf("partprobe: %w", err)
	}
	// Wait for the kernel/udev to create the partition device nodes.
	if err := waitForDevices(loopDev+"p1", loopDev+"p2"); err != nil {
		return err
	}

	// -------------------------------------------------------------------------
	// 6. Format partitions
	// -------------------------------------------------------------------------
	fmt.Println("Formatting partitions...")
	if err := run("mkfs.vfat", "-F32", loopDev+"p1"); err != nil {
		return fmt.Errorf("mkfs.vfat: %w", err)
	}
	if err := run("mkfs.ext4", "-F", loopDev+"p2"); err != nil {
		return fmt.Errorf("mkfs.ext4: %w", err)
	}

	fmt.Println("Setting filesystem labels...")
	if err := run("e2label", loopDev+"p2", "rootfs"); err != nil {
		return fmt.Errorf("e2label: %w", err)
	}
	if err := run("fatlabel", loopDev+"p1", "ESP"); err != nil {
		return fmt.Errorf("fatlabel: %w", err)
	}

	// -------------------------------------------------------------------------
	// 7. Start privileged container
	// -------------------------------------------------------------------------
	fmt.Println("Starting container with loop device access...")
	ctrID, err := runOutput("podman", "run", "-d", "--privileged",
		"--stop-signal=SIGKILL", // sleep as PID 1 ignores SIGTERM
		"--device", loopDev, "--entrypoint", "sleep", cfg.containerImage, "infinity")
	if err != nil {
		return fmt.Errorf("podman run: %w", err)
	}
	ctr = ctrID // assign so cleanup can remove it

	// -------------------------------------------------------------------------
	// 8. Detect distro inside container
	// -------------------------------------------------------------------------
	distroName, err := podmanExecOutput(ctr, "sh", "-c", `. /etc/os-release && printf "%s\n" "$NAME"`)
	if err != nil {
		return fmt.Errorf("reading container NAME: %w", err)
	}
	distroID, err := podmanExecOutput(ctr, "sh", "-c", `. /etc/os-release && printf "%s\n" "$ID"`)
	if err != nil {
		return fmt.Errorf("reading container ID: %w", err)
	}
	distroVersionID, err := podmanExecOutput(ctr, "sh", "-c", `. /etc/os-release && printf "%s\n" "$VERSION_ID"`)
	if err != nil {
		return fmt.Errorf("reading container VERSION_ID: %w", err)
	}
	fmt.Printf("Container distro release: %s (%s) %s\n", distroName, distroID, distroVersionID)

	// -------------------------------------------------------------------------
	// 9. Verify required packages
	// -------------------------------------------------------------------------
	fmt.Println("Ensure systemd, systemd-boot and kernel packages installed...")
	switch distroID {
	case "rocky":
		for _, pkg := range []string{"systemd", "systemd-boot-unsigned", "kernel", "dracut"} {
			if err := podmanExec(ctr, "dnf", "list", "--installed", pkg); err != nil {
				return fmt.Errorf("package %s not installed: %w", pkg, err)
			}
		}
		// Strip build-time dnf tweaks from the final image.
		if err := podmanExecShell(ctr, `sed -i '/keepcache=1/d' /etc/dnf/dnf.conf`); err != nil {
			return err
		}
		if err := podmanExecShell(ctr, `sed -i '/tsflags=nodocs/d' /etc/dnf/dnf.conf`); err != nil {
			return err
		}
	case "ubuntu":
		for _, pkg := range []string{"systemd", "systemd-boot", "linux-image-generic", "dracut"} {
			if err := podmanExec(ctr, "dpkg", "-l", pkg); err != nil {
				return fmt.Errorf("package %s not installed: %w", pkg, err)
			}
		}
	default:
		return fmt.Errorf("unsupported container distro: %s", distroID)
	}

	// -------------------------------------------------------------------------
	// 10. Find kernel version
	// -------------------------------------------------------------------------
	fmt.Println("Verifying kernel installation...")
	kernelVersion, err := podmanExecOutput(ctr, "bash", "-c", "ls /lib/modules/ | sort -V | tail -n1")
	if err != nil {
		return fmt.Errorf("listing kernels: %w", err)
	}
	if kernelVersion == "" {
		return errors.New("cannot find any kernels in /lib/modules")
	}
	fmt.Printf("Kernel version: %s\n", kernelVersion)

	var vmlinuzPath string
	switch distroID {
	case "rocky":
		vmlinuzPath = "/lib/modules/" + kernelVersion + "/vmlinuz"
	case "ubuntu":
		vmlinuzPath = "/boot/vmlinuz-" + kernelVersion
	}
	if _, err := podmanExecOutput(ctr, "ls", vmlinuzPath); err != nil {
		return fmt.Errorf("cannot find kernel file at %s: %w", vmlinuzPath, err)
	}
	fmt.Printf("Found kernel at: %s\n", vmlinuzPath)

	// -------------------------------------------------------------------------
	// 11. Regenerate initramfs
	// -------------------------------------------------------------------------
	fmt.Println("Regenerating initramfs with virtio drivers in container...")
	ctrInitramfsPath := "/boot/initramfs-" + kernelVersion + ".img"
	if err := podmanExec(ctr,
		"dracut", "--force",
		"--add-drivers", "virtio_blk virtio_pci virtio_scsi",
		"--filesystems", "ext4 vfat",
		ctrInitramfsPath, kernelVersion,
	); err != nil {
		return fmt.Errorf("dracut: %w", err)
	}
	if _, err := podmanExecOutput(ctr, "ls", ctrInitramfsPath); err != nil {
		return fmt.Errorf("dracut failed to create initramfs: %w", err)
	}

	// -------------------------------------------------------------------------
	// 12. Mount ESP and install systemd-boot
	// -------------------------------------------------------------------------
	fmt.Println("Mounting ESP in container...")
	if err := podmanExec(ctr, "mkdir", "-p", "/boot/efi"); err != nil {
		return fmt.Errorf("mkdir /boot/efi: %w", err)
	}
	if err := podmanExec(ctr, "mount", loopDev+"p1", "/boot/efi"); err != nil {
		return fmt.Errorf("mount ESP: %w", err)
	}
	if err := podmanExec(ctr, "mountpoint", "/boot/efi"); err != nil {
		return fmt.Errorf("ESP not mounted: %w", err)
	}

	fmt.Println("Installing systemd-boot in container...")
	if err := podmanExec(ctr, "bootctl", "install", "--esp-path=/boot/efi"); err != nil {
		return fmt.Errorf("bootctl install: %w", err)
	}

	// -------------------------------------------------------------------------
	// 13. Write boot loader configuration
	// -------------------------------------------------------------------------
	fmt.Println("Creating boot configuration in container...")
	bootEntry := fmt.Sprintf(
		"title %s %s\nlinux /vmlinuz-%s\ninitrd /initramfs-%s.img\noptions root=LABEL=rootfs ro console=tty0 console=ttyS0,115200n8\n",
		distroName, distroVersionID, kernelVersion, kernelVersion,
	)
	if err := podmanExecShell(ctr,
		fmt.Sprintf("cat > /boot/efi/loader/entries/%s.conf <<'EOF'\n%sEOF", distroID, bootEntry),
	); err != nil {
		return fmt.Errorf("writing boot entry: %w", err)
	}

	loaderConf := "default " + distroID + ".conf\ntimeout 3\nconsole-mode max\neditor no\n"
	if err := podmanExecShell(ctr,
		fmt.Sprintf("cat > /boot/efi/loader/loader.conf <<'EOF'\n%sEOF", loaderConf),
	); err != nil {
		return fmt.Errorf("writing loader.conf: %w", err)
	}

	// -------------------------------------------------------------------------
	// 14. Copy kernel and initramfs to ESP
	// -------------------------------------------------------------------------
	fmt.Println("Copying kernel files in container to ESP...")
	if err := podmanExec(ctr, "cp", vmlinuzPath, "/boot/efi/vmlinuz-"+kernelVersion); err != nil {
		return fmt.Errorf("copy vmlinuz: %w", err)
	}
	if err := podmanExec(ctr, "cp", ctrInitramfsPath, "/boot/efi/initramfs-"+kernelVersion+".img"); err != nil {
		return fmt.Errorf("copy initramfs: %w", err)
	}

	// -------------------------------------------------------------------------
	// 15. Write fstab
	// -------------------------------------------------------------------------
	fmt.Println("Creating fstab in container...")
	fstab := "LABEL=rootfs / ext4 defaults 1 1\nLABEL=ESP /boot/efi vfat defaults 0 2\n"
	if err := podmanExecShell(ctr,
		fmt.Sprintf("cat > /etc/fstab <<'EOF'\n%sEOF", fstab),
	); err != nil {
		return fmt.Errorf("writing fstab: %w", err)
	}

	// -------------------------------------------------------------------------
	// 16. Enable serial console (failure is non-fatal)
	// -------------------------------------------------------------------------
	fmt.Println("Enabling serial console in container...")
	_ = podmanExec(ctr, "systemctl", "enable", "serial-getty@ttyS0.service")

	// -------------------------------------------------------------------------
	// 17. Verify bootloader
	// -------------------------------------------------------------------------
	fmt.Println("Verifying bootloader installation...")
	if err := podmanExec(ctr, "ls", "-la", "/boot/efi/EFI/BOOT/"); err != nil {
		return fmt.Errorf("bootloader verification: %w", err)
	}

	// -------------------------------------------------------------------------
	// 18. Sanity-check sizes
	// -------------------------------------------------------------------------
	if err := podmanExec(ctr, "sync"); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	ctrRootfsBytesStr, err := podmanExecOutput(ctr, "bash", "-c", "du -sbx / | cut -f 1")
	if err != nil {
		return fmt.Errorf("du: %w", err)
	}
	ctrRootfsBytes, err := strconv.ParseInt(ctrRootfsBytesStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing du output %q: %w", ctrRootfsBytesStr, err)
	}
	rootPartBytesStr, err := runOutput("blockdev", "--getsize64", loopDev+"p2")
	if err != nil {
		return fmt.Errorf("blockdev: %w", err)
	}
	rootPartBytes, err := strconv.ParseInt(rootPartBytesStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing blockdev output %q: %w", rootPartBytesStr, err)
	}
	fmt.Printf("Container setup complete: %sB c.f. root partition of %sB\n",
		humanIEC(ctrRootfsBytes), humanIEC(rootPartBytes))

	// -------------------------------------------------------------------------
	// 19. Export container filesystem to root partition
	// -------------------------------------------------------------------------
	fmt.Println("Exporting container to root partition...")
	if _, err := os.Stat(cfg.rootMount); os.IsNotExist(err) {
		createdMount = true
	}
	if err := os.MkdirAll(cfg.rootMount, 0o755); err != nil {
		return fmt.Errorf("mkdir root mount: %w", err)
	}
	if err := run("mount", loopDev+"p2", cfg.rootMount); err != nil {
		return fmt.Errorf("mount root partition: %w", err)
	}
	rootMounted = true

	// Check against the space actually usable on the filesystem (ext4
	// metadata overhead means this is less than the raw partition size).
	var st syscall.Statfs_t
	if err := syscall.Statfs(cfg.rootMount, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", cfg.rootMount, err)
	}
	freeBytes := int64(st.Bfree) * int64(st.Bsize)
	if ctrRootfsBytes > freeBytes {
		return fmt.Errorf("container rootfs (%sB) larger than space available on root filesystem (%sB): increase -root-extra",
			humanIEC(ctrRootfsBytes), humanIEC(freeBytes))
	}

	// podman export | tar  (pipeline in Go: connect pipes manually)
	exportCmd := exec.Command("podman", "export", ctr)
	tarCmd := exec.Command("tar", "-C", cfg.rootMount, "--exclude=boot/efi/*", "--numeric-owner", "-xpf", "-")
	tarCmd.Stdout = os.Stdout
	tarCmd.Stderr = os.Stderr

	pipe, err := exportCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating export pipe: %w", err)
	}
	tarCmd.Stdin = pipe

	if err := exportCmd.Start(); err != nil {
		return fmt.Errorf("starting podman export: %w", err)
	}
	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("starting tar: %w", err)
	}
	// Wait for both so neither is left as a zombie; report the first failure.
	exportErr := exportCmd.Wait()
	tarErr := tarCmd.Wait()
	if exportErr != nil {
		return fmt.Errorf("podman export: %w", exportErr)
	}
	if tarErr != nil {
		return fmt.Errorf("tar extract: %w", tarErr)
	}

	// -------------------------------------------------------------------------
	// 20. Set file capabilities
	// -------------------------------------------------------------------------
	fmt.Println("Setting capabilities missing from container filesystem...")

	var capfiles []string
	switch distroID {
	case "rocky":
		capfiles = []string{
			"/usr/bin/arping:cap_net_raw=p",
			"/usr/bin/clockdiff:cap_net_raw=p",
			"/usr/bin/newgidmap:cap_setgid=ep",
			"/usr/bin/newuidmap:cap_setuid=ep",
		}
	case "ubuntu":
		capfiles = []string{
			"/usr/bin/mtr-packet:cap_net_raw=p",
			"/usr/bin/ping:cap_net_raw=p",
		}
	}
	capfiles = append(capfiles, strings.Fields(cfg.capfilesUser)...)

	for _, entry := range capfiles {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid capfiles entry: %q", entry)
		}
		hostPath := filepath.Join(cfg.rootMount, parts[0])
		// Lstat, not Stat: an absolute symlink inside the image would
		// otherwise resolve against the host filesystem.
		fi, err := os.Lstat(hostPath)
		if err != nil {
			continue // not an error for the file to be missing
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Printf("Skipping capability on symlink %s\n", parts[0])
			continue
		}
		if err := run("setcap", parts[1], hostPath); err != nil {
			return fmt.Errorf("setcap %s: %w", hostPath, err)
		}
	}

	// The export carries no SELinux xattrs, so have the first boot relabel.
	if distroID == "rocky" {
		if err := os.WriteFile(filepath.Join(cfg.rootMount, ".autorelabel"), nil, 0o644); err != nil {
			return fmt.Errorf("writing .autorelabel: %w", err)
		}
	}

	// Create /boot/efi mount point in exported filesystem.
	if err := os.MkdirAll(filepath.Join(cfg.rootMount, "boot", "efi"), 0o755); err != nil {
		return fmt.Errorf("mkdir boot/efi in rootfs: %w", err)
	}

	// -------------------------------------------------------------------------
	// 21. Final sync
	// -------------------------------------------------------------------------
	if err := run("sync"); err != nil {
		return fmt.Errorf("final sync: %w", err)
	}

	succeeded = true
	fmt.Printf("\nImage creation complete: %s\n\n", cfg.outputImage)
	return nil
}
