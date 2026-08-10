package cli

// Internal worker subcommands. The SSH layer runs these on a remote
// worker as `vci internal-*` instead of composing POSIX sh strings, so
// a Windows OpenSSH cmd.exe session never has to interpret `rm -rf`,
// `mkdir -p`, or `&&` chains. Every command speaks plain stdout/stderr
// and an exit code; the coordinator maps a non-zero exit to a
// remote-exit error. The reap command's `stale=N evicted=M` line is
// the only machine-readable output.

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/model"
)

// runInternalCommand dispatches one worker-side internal command with
// plain stdout/stderr and a process exit code — no JSON envelope. The
// detached worker's internal-run stays on the JSON envelope path.
func runInternalCommand(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	code, err := runInternal(name, args, stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "vci %s: %v\n", name, err)
		return 1
	}
	return code
}

// runInternal executes one internal command body. A nil error with a
// non-zero code is an intentional non-error exit (the probe miss).
func runInternal(name string, args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	switch name {
	case "internal-stage":
		if len(args) != 1 {
			return 0, fmt.Errorf("usage: internal-stage <workDir>")
		}
		if err := internalStage(args[0], stdin); err != nil {
			return 0, err
		}
		return 0, nil
	case "internal-probe-cache":
		if len(args) != 1 {
			return 0, fmt.Errorf("usage: internal-probe-cache <entry>")
		}
		found, err := internalProbeCache(args[0])
		if err != nil {
			return 0, err
		}
		if !found {
			return 1, nil
		}
		return 0, nil
	case "internal-acquire-claim":
		if len(args) != 2 {
			return 0, fmt.Errorf("usage: internal-acquire-claim <entry> <claimID>")
		}
		return 0, internalAcquireClaim(args[0], args[1])
	case "internal-release-claim":
		if len(args) != 2 {
			return 0, fmt.Errorf("usage: internal-release-claim <entry> <claimID>")
		}
		return 0, internalReleaseClaim(args[0], args[1])
	case "internal-reap-cache":
		if len(args) != 8 {
			return 0, fmt.Errorf("usage: internal-reap-cache <projDir> <refDir> <partialRef> <claimRef> <partialCutoff> <claimCutoff> <maxEntries> <maxBytes>")
		}
		partialCutoff, err := time.Parse(time.RFC3339, args[4])
		if err != nil {
			return 0, fmt.Errorf("invalid partial cutoff %q: %v", args[4], err)
		}
		claimCutoff, err := time.Parse(time.RFC3339, args[5])
		if err != nil {
			return 0, fmt.Errorf("invalid claim cutoff %q: %v", args[5], err)
		}
		maxEntries, err := strconv.Atoi(args[6])
		if err != nil || maxEntries < 0 {
			return 0, fmt.Errorf("invalid maxEntries %q", args[6])
		}
		maxBytes, err := strconv.ParseInt(args[7], 10, 64)
		if err != nil || maxBytes < 0 {
			return 0, fmt.Errorf("invalid maxBytes %q", args[7])
		}
		stale, evicted, err := internalReapCache(args[0], args[1], args[2], args[3], partialCutoff, claimCutoff, maxEntries, maxBytes)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "stale=%d evicted=%d\n", stale, evicted)
		return 0, nil
	case "internal-reconstruct":
		return 0, internalReconstruct(args, stdin)
	default:
		return 0, fmt.Errorf("unknown internal command %q", name)
	}
}

// internalStage clears the work dir and extracts a workspace tar from
// stdin into it, replacing the shell `rm -rf && mkdir -p && tar -xpf -`
// composition.
func internalStage(workDir string, stdin io.Reader) error {
	dir, err := resolveWorkerWorkDir(workDir)
	if err != nil {
		return err
	}
	if stdin == nil {
		return fmt.Errorf("stage archive is required on stdin")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := extractTar(stdin, dir); err != nil {
		return fmt.Errorf("extract staged workspace: %w", err)
	}
	return nil
}

// internalProbeCache reports whether a worker cache entry is complete,
// replacing the shell `test -f <entry>/complete`.
func internalProbeCache(entry string) (bool, error) {
	dir, err := resolveWorkerCacheEntry(entry)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(dir, "complete"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// internalAcquireClaim writes an active-claim marker under a complete
// cache entry, replacing the shell `test -f ... && mkdir -p claims &&
// : > claims/<id>`.
func internalAcquireClaim(entry, claimID string) error {
	dir, err := resolveWorkerCacheEntry(entry)
	if err != nil {
		return err
	}
	if !host.ValidCacheSegment(claimID) {
		return fmt.Errorf("claim id %q must be a single path segment of letters, digits, '.', '-', '_'", claimID)
	}
	if _, err := os.Stat(filepath.Join(dir, "complete")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cache entry %s is not complete", dir)
		}
		return err
	}
	claims := filepath.Join(dir, "claims")
	if err := os.MkdirAll(claims, 0o755); err != nil {
		return err
	}
	marker, err := os.Create(filepath.Join(claims, claimID))
	if err != nil {
		return err
	}
	return marker.Close()
}

// internalReleaseClaim removes an active-claim marker, replacing the
// shell `rm -f claims/<id>`. A missing marker is not an error.
func internalReleaseClaim(entry, claimID string) error {
	dir, err := resolveWorkerCacheEntry(entry)
	if err != nil {
		return err
	}
	if !host.ValidCacheSegment(claimID) {
		return fmt.Errorf("claim id %q must be a single path segment of letters, digits, '.', '-', '_'", claimID)
	}
	err = os.Remove(filepath.Join(dir, "claims", claimID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// internalReapCache runs the worker bundle-cache maintenance pass for
// one project: incomplete entries and claim markers untouched for their
// cutoff windows are removed, then claim-free complete entries beyond
// the policy limits are evicted oldest-first. The two cutoffs arrive as
// RFC3339 timestamps and are stamped onto reference files so staleness
// uses the same mtime comparison the shell's `touch -t` cutoffs did.
// The reference files and their directory are removed before returning.
// Every removal aborts on failure so an undeletable entry cannot
// silently under-count.
func internalReapCache(projDir, refDir, partialRef, claimRef string, partialCutoff, claimCutoff time.Time, maxEntries int, maxBytes int64) (int, int, error) {
	proj, err := resolveWorkerCacheProjectDir(projDir)
	if err != nil {
		return 0, 0, err
	}
	if refDir != proj+"/.vci-reap" || partialRef != refDir+"/partial" || claimRef != refDir+"/claim" {
		return 0, 0, fmt.Errorf("reap reference paths do not derive from the project dir")
	}
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		return 0, 0, err
	}
	if err := writeCutoffRef(partialRef, partialCutoff); err != nil {
		return 0, 0, err
	}
	if err := writeCutoffRef(claimRef, claimCutoff); err != nil {
		return 0, 0, err
	}
	partialInfo, err := os.Stat(partialRef)
	if err != nil {
		return 0, 0, err
	}
	claimInfo, err := os.Stat(claimRef)
	if err != nil {
		return 0, 0, err
	}
	partialMod := partialInfo.ModTime()
	claimMod := claimInfo.ModTime()

	stale := 0
	entries, err := os.ReadDir(proj)
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	for _, e := range entries {
		d := filepath.Join(proj, e.Name())
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		if fi, err := os.Stat(filepath.Join(d, "complete")); err == nil && fi.Mode().IsRegular() {
			continue
		}
		var mod time.Time
		if metaInfo, err := os.Stat(filepath.Join(d, "meta.json")); err == nil {
			mod = metaInfo.ModTime()
		} else {
			mod = info.ModTime()
		}
		if !mod.Before(partialMod) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return 0, 0, fmt.Errorf("remove stale entry %s: %w", d, err)
		}
		stale++
	}

	for _, e := range entries {
		claims := filepath.Join(proj, e.Name(), "claims")
		claimEntries, err := os.ReadDir(claims)
		if err != nil {
			continue
		}
		for _, c := range claimEntries {
			marker := filepath.Join(claims, c.Name())
			info, err := os.Stat(marker)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if !info.ModTime().Before(claimMod) {
				continue
			}
			if err := os.Remove(marker); err != nil {
				return 0, 0, fmt.Errorf("remove stale claim %s: %w", marker, err)
			}
			stale++
		}
	}

	evicted, err := evictLRU(proj, maxEntries, maxBytes)
	if err != nil {
		return 0, 0, err
	}

	if err := os.Remove(partialRef); err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	if err := os.Remove(claimRef); err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	if err := os.Remove(refDir); err != nil {
		return 0, 0, err
	}
	return stale, evicted, nil
}

// writeCutoffRef creates an empty reference file whose mtime marks the
// cutoff of one stale window, mirroring the shell's `TZ=UTC touch -t`.
func writeCutoffRef(path string, cutoff time.Time) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(path, cutoff, cutoff)
}

// evictLRU removes claim-free complete entries of projDir, oldest
// meta.json first, until both positive limits are satisfied. Limits of
// zero are skipped. A removal failure aborts so an undeletable entry
// cannot silently under-count.
func evictLRU(projDir string, maxEntries int, maxBytes int64) (int, error) {
	if maxEntries <= 0 && maxBytes <= 0 {
		return 0, nil
	}
	evicted := 0
	for {
		entries, err := os.ReadDir(projDir)
		if err != nil {
			if os.IsNotExist(err) {
				return evicted, nil
			}
			return 0, err
		}
		n, b := 0, int64(0)
		for _, e := range entries {
			d := filepath.Join(projDir, e.Name())
			if !hasComplete(d) {
				continue
			}
			n++
			bytes, _, _ := entryMeta(d)
			b += bytes
		}
		if (maxEntries <= 0 || n <= maxEntries) && (maxBytes <= 0 || b <= maxBytes) {
			return evicted, nil
		}
		oldest := ""
		var oldestMod time.Time
		for _, e := range entries {
			d := filepath.Join(projDir, e.Name())
			if !hasComplete(d) || claimsNonEmpty(d) {
				continue
			}
			_, mod, ok := entryMeta(d)
			if oldest == "" {
				// The shell's first claim-free complete entry wins
				// until a strictly older meta.json replaces it.
				oldest = d
				if ok {
					oldestMod = mod
				}
				continue
			}
			if !ok || oldestMod.IsZero() {
				continue
			}
			if mod.Before(oldestMod) {
				oldest, oldestMod = d, mod
			}
		}
		if oldest == "" {
			return evicted, nil
		}
		if err := os.RemoveAll(oldest); err != nil {
			return 0, fmt.Errorf("remove LRU entry %s: %w", oldest, err)
		}
		evicted++
	}
}

// hasComplete reports whether d carries a complete marker, mirroring
// the shell's `[ -f "$d/complete" ]`.
func hasComplete(d string) bool {
	info, err := os.Stat(filepath.Join(d, "complete"))
	return err == nil && info.Mode().IsRegular()
}

// claimsNonEmpty reports whether d has any claim marker, mirroring the
// shell's `[ -n "$(ls -A "$d/claims" 2>/dev/null)" ]`.
func claimsNonEmpty(d string) bool {
	entries, err := os.ReadDir(filepath.Join(d, "claims"))
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// entryMeta reads the byte size and meta.json mtime of a complete
// entry. ok is false when meta.json is missing or unreadable; such an
// entry counts zero bytes and never ranks as the LRU candidate unless
// it is the only claim-free entry.
func entryMeta(d string) (bytes int64, mod time.Time, ok bool) {
	metaPath := filepath.Join(d, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return 0, time.Time{}, false
	}
	var meta struct {
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, time.Time{}, false
	}
	info, err := os.Stat(metaPath)
	if err != nil {
		return meta.Bytes, time.Time{}, false
	}
	return meta.Bytes, info.ModTime(), true
}

// extractTar writes every member of r under root, preserving modes and
// symlinks and refusing any name that would escape root.
func extractTar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		target, err := safeJoin(root, h.Name)
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeLink {
			linkTarget, err := safeJoin(root, h.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
			continue
		}
		if err := extractMember(tr, h, target); err != nil {
			return fmt.Errorf("extract %s: %w", h.Name, err)
		}
	}
}

// extractMember writes one tar member to target, preserving modes and
// symlinks. Special files are skipped; Vci workspaces never carry them.
func extractMember(r io.Reader, h *tar.Header, target string) error {
	switch h.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, h.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Chmod(target, h.FileInfo().Mode().Perm())
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(h.Linkname, target)
	default:
		return nil
	}
}

// safeJoin joins one archive member name onto root, rejecting absolute
// names and any `..` segment so a hostile archive cannot escape the
// extraction root.
func safeJoin(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("archive member %q is not a relative path", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive member %q escapes the extraction root", name)
	}
	return filepath.Join(root, clean), nil
}

// ---- Worker-side path resolution ----

// The coordinator always composes remote paths in `~/.vci/...` form. A
// Unix login shell expands the tilde before vci starts, while a Windows
// cmd.exe session passes it through untouched, so the worker resolves a
// leading `~` itself and then validates the absolute result.

// expandWorkerPath resolves a leading `~` to the worker's home.
func expandWorkerPath(p string) (string, error) {
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve worker home: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve worker home: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~/")), nil
	}
	return p, nil
}

// safePathPrefix rejects empty, option-like, and control-bearing paths
// before any filesystem access.
func safePathPrefix(what, p string) error {
	if p == "" {
		return fmt.Errorf("%s path is empty", what)
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("%s path %q looks like an option flag", what, p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s path %q contains control characters", what, p)
		}
	}
	return nil
}

// resolveWorkerWorkDir validates a worker work path and returns its
// absolute form. The path must end in `.vci/state/work/<run_id>` so a
// stage or reconstruction can never touch state outside Vci ownership.
func resolveWorkerWorkDir(p string) (string, error) {
	if err := safePathPrefix("work", p); err != nil {
		return "", err
	}
	expanded, err := expandWorkerPath(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("work path %q is not absolute", p)
	}
	segs := strings.Split(filepath.ToSlash(filepath.Clean(expanded)), "/")
	if len(segs) < 5 {
		return "", fmt.Errorf("work path %q is not under .vci/state/work", p)
	}
	run := segs[len(segs)-1]
	if segs[len(segs)-2] != "work" || segs[len(segs)-3] != "state" || segs[len(segs)-4] != ".vci" || !model.ValidRunID(model.RunID(run)) {
		return "", fmt.Errorf("work path %q is not under .vci/state/work", p)
	}
	return filepath.Clean(expanded), nil
}

// resolveWorkerCacheEntry validates a worker cache entry path and
// returns its absolute form. The path must end in
// `.vci/state/bundle-cache/v1/<project>/<base>` with safe segments.
func resolveWorkerCacheEntry(p string) (string, error) {
	if err := safePathPrefix("cache entry", p); err != nil {
		return "", err
	}
	expanded, err := expandWorkerPath(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("cache entry path %q is not absolute", p)
	}
	segs := strings.Split(filepath.ToSlash(filepath.Clean(expanded)), "/")
	if len(segs) < 6 {
		return "", fmt.Errorf("cache entry path %q is not under .vci/state/bundle-cache", p)
	}
	project := segs[len(segs)-2]
	base := segs[len(segs)-1]
	if segs[len(segs)-6] != ".vci" || segs[len(segs)-5] != "state" || segs[len(segs)-4] != "bundle-cache" || segs[len(segs)-3] != "v1" || !host.ValidCacheSegment(project) || !host.ValidCacheSegment(base) {
		return "", fmt.Errorf("cache entry path %q is not under .vci/state/bundle-cache", p)
	}
	return filepath.Clean(expanded), nil
}

// resolveWorkerCacheProjectDir validates a worker cache project dir and
// returns its absolute form. The path must end in
// `.vci/state/bundle-cache/v1/<project>`.
func resolveWorkerCacheProjectDir(p string) (string, error) {
	if err := safePathPrefix("cache project", p); err != nil {
		return "", err
	}
	expanded, err := expandWorkerPath(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("cache project path %q is not absolute", p)
	}
	segs := strings.Split(filepath.ToSlash(filepath.Clean(expanded)), "/")
	if len(segs) < 5 {
		return "", fmt.Errorf("cache project path %q is not under .vci/state/bundle-cache", p)
	}
	project := segs[len(segs)-1]
	if segs[len(segs)-5] != ".vci" || segs[len(segs)-4] != "state" || segs[len(segs)-3] != "bundle-cache" || segs[len(segs)-2] != "v1" || !host.ValidCacheSegment(project) {
		return "", fmt.Errorf("cache project path %q is not under .vci/state/bundle-cache", p)
	}
	return filepath.Clean(expanded), nil
}

// resolveWorkerSeed validates a worker seed path and resolves a leading
// `~`. The seed is a coordinator-configured checkout path, so only the
// shared character rules apply.
func resolveWorkerSeed(p string) (string, error) {
	if err := safePathPrefix("seed", p); err != nil {
		return "", err
	}
	return expandWorkerPath(p)
}
