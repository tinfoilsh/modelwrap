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
func seedFromPreviousRevisions(name, revision, modelDir, token string) {
	siblings, err := siblingRevisionDirs(modelDir)
	if err != nil || len(siblings) == 0 {
		return
	}
	files, err := listLFSFiles(name, revision, token)
	if err != nil {
		fmt.Printf("Not seeding %s from previous revisions: %v\n", revision, err)
		return
	}
	linked, linkedBytes, sources := seedFiles(files, siblings, modelDir)
	if linked > 0 {
		fmt.Printf("Seeded %d of %d LFS files (%s) from %s; downloading the remaining %d\n",
			linked, len(files), formatSize(linkedBytes), strings.Join(sources, ", "), len(files)-linked)
	}
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
	for pageURL != "" {
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
		err = json.NewDecoder(resp.Body).Decode(&page)
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

// seedFiles hardlinks each wanted file from the first sibling revision
// whose copy matches the expected SHA256, hashing candidates in parallel
// across a worker pool. Files already present in modelDir are left for hf
// to resolve. Returns the linked file count, their total size, and the
// sibling revisions used.
func seedFiles(files []lfsFile, siblings []string, modelDir string) (int, int64, []string) {
	type result struct {
		linked bool
		size   int64
		source string
	}
	results := make([]result, len(files))

	var next atomic.Uint64
	var wg sync.WaitGroup
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
					if err != nil || !fi.Mode().IsRegular() || fi.Size() != file.Size {
						continue
					}
					if !fileMatchesSHA256(candidate, file.SHA256, buf) {
						continue
					}
					if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
						break
					}
					// Same filesystem by construction; if linking still
					// fails, download instead — never copy silently.
					if err := os.Link(candidate, target); err != nil {
						break
					}
					results[t] = result{true, file.Size, filepath.Base(sibling)}
					break
				}
			}
		}()
	}
	wg.Wait()

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
	return linked, linkedBytes, sources
}

// fileMatchesSHA256 reports whether the full content of the file at path
// hashes to wantHex.
func fileMatchesSHA256(path, wantHex string, buf []byte) bool {
	f, err := os.Open(path)
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
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(wantHex)
}
