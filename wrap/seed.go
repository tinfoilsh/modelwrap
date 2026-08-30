package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hfAPIBase is a variable so tests can point Hub API calls at a stub server.
var hfAPIBase = "https://huggingface.co/api"

// lfsFile is one LFS-tracked file of a model revision, from the Hub tree
// API. Only LFS files are seeded: the API exposes their content SHA256,
// while regular files carry only a git blob oid and are cheap to download.
type lfsFile struct {
	Path   string
	SHA256 string
	Size   int64
}

// seedFromPreviousRevisions hardlinks into modelDir the LFS files that an
// already-downloaded revision of the same model shares with revision, each
// verified by content hash against the Hub-reported SHA256, so `hf download`
// only fetches what actually changed. Seeding is purely an optimization and
// never a correctness gate: any failure falls back to the plain download,
// and hf independently re-hashes files it finds without sidecar metadata
// before skipping them.
//
// The hardlink model relies on hf never writing through an existing
// destination path: the pinned huggingface_hub replaces files by renaming
// a .incomplete temp over them. Its hub-cache-hit branch is the exception
// — it shutil.copyfile()s onto the destination in place, which would
// corrupt the seeded sibling through the shared inode. That branch is
// unreachable in the packer container (ephemeral, --local-dir downloads
// only, the hub cache is never populated); do not enable hub-cache reuse
// without revisiting seeding.
// It returns an error only when it leaves the target dir in a state that
// must not be packed; every other failure just skips seeding.
func seedFromPreviousRevisions(name, revision, modelDir, token string) error {
	siblings, err := siblingRevisionDirs(modelDir)
	if err != nil || len(siblings) == 0 {
		return nil
	}
	mode, err := probeDownloadMode(modelDir)
	if err != nil {
		fmt.Printf("Not seeding %s from previous revisions: probing download mode: %v\n", revision, err)
		return nil
	}
	files, err := listLFSFiles(name, revision, token)
	if err != nil {
		fmt.Printf("Not seeding %s from previous revisions: %v\n", revision, err)
		return nil
	}
	linked, linkedBytes, sources, err := seedFiles(files, siblings, modelDir, mode)
	if err != nil {
		return err
	}
	if linked > 0 {
		fmt.Printf("Seeded %d of %d LFS files (%s) from %s; downloading the remaining %d\n",
			linked, len(files), formatSize(linkedBytes), strings.Join(sources, ", "), len(files)-linked)
	}
	return nil
}

func formatSize(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
}

// siblingRevisionDirs lists the other revision directories already present
// for the same model, most recently modified first.
func siblingRevisionDirs(modelDir string) ([]string, error) {
	parent := filepath.Dir(modelDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type revDir struct {
		path string
		mod  time.Time
	}
	var dirs []revDir
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == filepath.Base(modelDir) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, revDir{filepath.Join(parent, entry.Name()), info.ModTime()})
	}
	slices.SortFunc(dirs, func(a, b revDir) int { return b.mod.Compare(a.mod) })
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = d.path
	}
	return paths, nil
}

// Bounds on the tree listing so a misbehaving API response degrades into
// a listing error (and therefore a plain unseeded download) instead of a
// wedged or OOMing wrap job.
const (
	treeMaxPages    = 10000
	treePageMaxSize = 64 << 20
)

// listLFSFiles enumerates the LFS files of one model revision via the Hub
// tree API, following Link header pagination.
func listLFSFiles(name, revision, token string) ([]lfsFile, error) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	pageURL := hfAPIBase + "/models/" + strings.Join(segments, "/") +
		"/tree/" + url.PathEscape(revision) + "?recursive=true"

	client := &http.Client{Timeout: 2 * time.Minute}
	var files []lfsFile
	for pages := 0; pageURL != ""; pages++ {
		if pages == treeMaxPages {
			return nil, fmt.Errorf("listing files of %s@%s: more than %d pages", name, revision, treeMaxPages)
		}
		req, err := http.NewRequest("GET", pageURL, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing files of %s@%s: %w", name, revision, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("listing files of %s@%s: %s", name, revision, resp.Status)
		}
		var page []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			LFS  *struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, treePageMaxSize)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decoding file list of %s@%s: %w", name, revision, err)
		}
		for _, entry := range page {
			if entry.Type != "file" || entry.LFS == nil || !filepath.IsLocal(entry.Path) {
				continue
			}
			files = append(files, lfsFile{Path: entry.Path, SHA256: entry.LFS.OID, Size: entry.LFS.Size})
		}
		pageURL = nextPageURL(resp.Header)
	}
	return files, nil
}

// nextPageURL extracts the rel="next" target from a Link response header.
func nextPageURL(header http.Header) string {
	for _, field := range header.Values("Link") {
		for _, link := range strings.Split(field, ",") {
			target, params, ok := strings.Cut(link, ";")
			if ok && strings.Contains(params, `rel="next"`) {
				return strings.Trim(strings.TrimSpace(target), "<>")
			}
		}
	}
	return ""
}

// probeDownloadMode returns the full file mode hf gives files it
// downloads here (a regular file, 0666 &^ the process umask, no special
// bits), determined the way huggingface_hub itself probes it: by creating
// a scratch file. Only files with exactly this mode may be seeded —
// mkfs.erofs stores the whole inode mode, so a sibling copy from a
// different-umask era, or one with a setuid/setgid/sticky bit, would fork
// the seeded roothash from a cold wrap's. Chmod is not an option: it
// would write through the shared inode into the sibling revision.
// The probe lives under the revision's .cache dir, which Pack always
// removes before the image is built, so it can never leak into the
// artifact even if a killed run leaves it behind.
func probeDownloadMode(modelDir string) (os.FileMode, error) {
	// 0777 &^ umask matches how hf's pathlib mkdir creates missing dirs,
	// so directories the seeder creates first (including the packed model
	// root) get the same stored mode a cold wrap's would.
	cacheDir := filepath.Join(modelDir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return 0, err
	}
	probe := filepath.Join(cacheDir, ".seed-mode-probe")
	if err := os.Remove(probe); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o666)
	if err != nil {
		return 0, err
	}
	defer os.Remove(probe)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Mode(), nil
}

// removeSeedTarget is a variable so tests can force the unremovable-file
// failure path.
var removeSeedTarget = os.Remove

// seedFiles hardlinks each wanted file from the first sibling revision
// whose copy matches the expected SHA256, hashing candidates in parallel
// across a worker pool. Files already present in modelDir are left for hf
// to resolve. Returns the linked file count, their total size, and the
// sibling revisions used. The only error is an unverified link that could
// not be removed again; it must fail the wrap.
func seedFiles(files []lfsFile, siblings []string, modelDir string, mode os.FileMode) (int, int64, []string, error) {
	type result struct {
		linked bool
		size   int64
		source string
	}
	results := make([]result, len(files))

	var (
		next     atomic.Uint64
		errOnce  sync.Once
		firstErr error
		wg       sync.WaitGroup
	)
	for range min(runtime.GOMAXPROCS(0), len(files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			for {
				t := next.Add(1) - 1
				if t >= uint64(len(files)) {
					return
				}
				file := files[t]
				target := filepath.Join(modelDir, filepath.FromSlash(file.Path))
				if _, err := os.Lstat(target); err == nil {
					continue
				}
				for _, sibling := range siblings {
					candidate := filepath.Join(sibling, filepath.FromSlash(file.Path))
					fi, err := os.Lstat(candidate)
					if err != nil || fi.Mode() != mode || fi.Size() != file.Size {
						continue
					}
					// 0777 &^ umask matches hf's own dir creation, see
					// probeDownloadMode.
					if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
						continue
					}
					// Link first, verify after: every check runs on the
					// pinned target inode, so a concurrent re-download
					// replacing the sibling path cannot swap in an
					// unverified file between hash and link. Same
					// filesystem by construction; if linking fails,
					// download instead — never copy silently.
					if err := os.Link(candidate, target); err != nil {
						continue
					}
					if !seedTargetValid(target, file, mode, buf) {
						// The one case where seeding must abort the wrap
						// rather than degrade: an unverified file left in
						// the target dir would be packed as if hf had
						// produced it (hf trusts existing files when its
						// metadata request fails), poisoning the artifact.
						if rmErr := removeSeedTarget(target); rmErr != nil {
							errOnce.Do(func() {
								firstErr = fmt.Errorf("seeding left an unverified file it could not remove: %w; aborting to protect pack integrity", rmErr)
							})
							return
						}
						continue
					}
					results[t] = result{true, file.Size, filepath.Base(sibling)}
					break
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, 0, nil, firstErr
	}

	var linked int
	var linkedBytes int64
	used := make(map[string]bool)
	for _, r := range results {
		if r.linked {
			linked++
			linkedBytes += r.size
			used[r.source] = true
		}
	}
	var sources []string
	for _, sibling := range siblings {
		if used[filepath.Base(sibling)] {
			sources = append(sources, filepath.Base(sibling))
		}
	}
	return linked, linkedBytes, sources, nil
}

// seedTargetValid reports whether the freshly linked seed target has
// exactly the probed full mode (which subsumes being a regular file
// without special bits) and expected size, and content that hashes to the
// expected SHA256.
func seedTargetValid(target string, file lfsFile, mode os.FileMode, buf []byte) bool {
	fi, err := os.Lstat(target)
	if err != nil || fi.Mode() != mode || fi.Size() != file.Size {
		return false
	}
	f, err := os.Open(target)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	for {
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(file.SHA256)
}
