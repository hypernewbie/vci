package cli

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Worker-side workspace reconstruction. The SSH layer runs these bodies on a
// remote worker as `vci internal-reconstruct`; the subcommand reads a payload
// tar on stdin, seeds the workspace from a checkout or the worker bundle
// cache, checks out the submitted head, applies the durable local-change
// archive, and optionally admits the transferred bundle into the cache.

// internalReconstruct turns a streamed worker payload into a
// reconstructed workspace: extract the payload into
// <workDir>/.vci-payload, copy the seed checkout into workDir (or skip
// it with --no-seed), initialize the repository, import the cache entry
// bundle and/or the payload bundle best-effort, check out the submitted
// head, apply the durable local-change archive, admit the bundle into
// the worker cache and evict LRU entries on request, then remove the
// payload. Any step failure aborts so a half-built workspace is never
// left behind.
func internalReconstruct(args []string, stdin io.Reader) error {
	var entry string
	var useCached, admit, evict, noSeed bool
	var bundleBytes int64
	var lastUsed time.Time
	var maxEntries int
	var maxBytes int64
	var positionals []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-seed":
			noSeed = true
		case "--use-cached":
			useCached = true
		case "--cache":
			i++
			if i >= len(args) {
				return fmt.Errorf("--cache requires a value")
			}
			entry = args[i]
		case "--admit":
			i++
			if i+1 >= len(args) {
				return fmt.Errorf("--admit requires <bundleBytes> <rfc3339>")
			}
			n, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("invalid bundle bytes %q", args[i])
			}
			bundleBytes = n
			t, err := time.Parse(time.RFC3339, args[i+1])
			if err != nil {
				return fmt.Errorf("invalid last-used time %q", args[i+1])
			}
			lastUsed = t
			admit = true
			i++
		case "--evict":
			i++
			if i+1 >= len(args) {
				return fmt.Errorf("--evict requires <maxEntries> <maxBytes>")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				return fmt.Errorf("invalid maxEntries %q", args[i])
			}
			maxEntries = n
			mb, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || mb < 0 {
				return fmt.Errorf("invalid maxBytes %q", args[i+1])
			}
			maxBytes = mb
			evict = true
			i++
		default:
			positionals = append(positionals, args[i])
		}
	}
	if len(positionals) < 1 || len(positionals) > 2 {
		return fmt.Errorf("usage: internal-reconstruct <workDir> [<seed>] [--no-seed] [--cache <entry> [--use-cached] [--admit <bundleBytes> <rfc3339> [--evict <maxEntries> <maxBytes>]]]")
	}
	workDir, err := resolveWorkerWorkDir(positionals[0])
	if err != nil {
		return err
	}
	seed := ""
	if len(positionals) == 2 {
		seed = positionals[1]
	}
	if !noSeed && seed == "" {
		return fmt.Errorf("seed path is required unless --no-seed is given")
	}
	if !noSeed {
		if seed, err = resolveWorkerSeed(seed); err != nil {
			return err
		}
	}
	if entry != "" {
		if entry, err = resolveWorkerCacheEntry(entry); err != nil {
			return err
		}
	}
	if useCached && entry == "" {
		return fmt.Errorf("--use-cached requires --cache")
	}
	if admit && entry == "" {
		return fmt.Errorf("--admit requires --cache")
	}
	if evict && entry == "" {
		return fmt.Errorf("--evict requires --cache")
	}
	if stdin == nil {
		return fmt.Errorf("reconstruction payload is required on stdin")
	}

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	payloadDir := filepath.Join(workDir, ".vci-payload")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return err
	}
	if err := extractTar(stdin, payloadDir); err != nil {
		return fmt.Errorf("extract payload: %w", err)
	}
	if !noSeed {
		if err := copyTree(seed, workDir); err != nil {
			return fmt.Errorf("copy seed: %w", err)
		}
	}
	if err := runGit(workDir, "init", "-q"); err != nil {
		return err
	}
	if useCached {
		// The cached bundle is the base; a payload delta extends it.
		// Both imports are best-effort because the checkout step below
		// detects a genuinely missing history.
		_ = runGit(workDir, "bundle", "unbundle", filepath.Join(entry, "bundle"))
		if info, err := os.Stat(filepath.Join(payloadDir, "bundle")); err == nil && info.Size() > 0 {
			_ = runGit(workDir, "bundle", "unbundle", filepath.Join(payloadDir, "bundle"))
		}
		// Refresh the entry's recency so a hit counts as recent use.
		if err := os.Chtimes(filepath.Join(entry, "meta.json"), time.Now(), time.Now()); err != nil {
			return err
		}
	} else {
		_ = runGit(workDir, "bundle", "unbundle", filepath.Join(payloadDir, "bundle"))
	}
	headData, err := os.ReadFile(filepath.Join(payloadDir, "head"))
	if err != nil {
		return fmt.Errorf("read submitted head: %w", err)
	}
	head := strings.TrimSpace(string(headData))
	if head == "" {
		return fmt.Errorf("submitted head is empty")
	}
	if err := runGit(workDir, "checkout", "-q", head); err != nil {
		return err
	}
	lcPath := filepath.Join(payloadDir, "lc.tar")
	if info, err := os.Stat(lcPath); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("local-change archive is missing")
	}
	if err := applyLocalChanges(workDir, payloadDir, lcPath); err != nil {
		return err
	}
	if admit {
		if err := admitBundle(entry, filepath.Join(payloadDir, "bundle"), bundleBytes, lastUsed); err != nil {
			return err
		}
		if evict {
			if _, err := evictLRU(filepath.Dir(entry), maxEntries, maxBytes); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(payloadDir)
}

// admitBundle writes the streamed payload bundle into a cache entry and
// marks it complete. The complete marker is written last so an
// interrupted admission is never a hit, and meta.json carries the byte
// size and last-used timestamp in the bundlecache shape.
func admitBundle(entry, bundlePath string, bundleBytes int64, lastUsed time.Time) error {
	if err := os.MkdirAll(entry, 0o755); err != nil {
		return err
	}
	src, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(entry, "bundle"))
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	meta := fmt.Sprintf(`{"bytes":%d,"last_used":%q}`, bundleBytes, lastUsed.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(entry, "meta.json"), []byte(meta), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(entry, "complete"), nil, 0o644)
}

// applyLocalChanges consumes the durable local-change archive: the
// patch member is applied before the f/ tree is extracted so an
// untracked file named "patch" cannot collide with the staging patch.
func applyLocalChanges(workDir, payloadDir, lcPath string) error {
	f, err := os.Open(lcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read local-change archive: %w", err)
		}
		if h.Name != "patch" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		patchPath := filepath.Join(payloadDir, "patch")
		if err := os.WriteFile(patchPath, data, 0o644); err != nil {
			return err
		}
		if err := runGit(workDir, "apply", "--binary", "--whitespace=nowarn", patchPath); err != nil {
			return err
		}
	}

	f2, err := os.Open(lcPath)
	if err != nil {
		return err
	}
	defer f2.Close()
	tr2 := tar.NewReader(f2)
	for {
		h, err := tr2.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read local-change archive: %w", err)
		}
		if !strings.HasPrefix(h.Name, "f/") {
			continue
		}
		rel := strings.TrimPrefix(h.Name, "f/")
		target, err := safeJoin(workDir, rel)
		if err != nil {
			return err
		}
		if err := extractMember(tr2, h, target); err != nil {
			return fmt.Errorf("extract local change %s: %w", h.Name, err)
		}
	}
}

// copyTree recursively copies src into dst, preserving modes and
// symlinks. The seed's own `.vci` state directory and the destination
// tree itself are skipped so a seed that already contains a workspace
// cannot recurse into it.
func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dst || strings.HasPrefix(path, dst+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".vci" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return copyFile(path, target, info.Mode().Perm())
		default:
			// Devices, fifos, and sockets are not part of a build
			// checkout; skip them.
			return nil
		}
	})
}

// copyFile copies one regular file, preserving its mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// runGit runs `git -C dir args...` and reports failure with the
// command's output.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
