// Command naive downloads a Hugging Face model using only the Go standard
// library: it lists the repo file tree via the Hub API and fetches each
// file over plain HTTPS (following the resolve redirects to the CDN), one
// file at a time.
//
// It is the simplest possible "no supply chain" baseline against the
// official hf CLI (huggingface_hub + hf_xet), which modelwrap currently
// shells out to. No Python, no huggingface_hub, no Xet plugin, no
// concurrency — just HTTP, sequentially.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type entry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

const hub = "https://huggingface.co"

func main() {
	repo := flag.String("repo", "", "Hugging Face repo id, e.g. Qwen/Qwen2.5-72B-Instruct")
	rev := flag.String("revision", "main", "revision (branch or commit)")
	out := flag.String("out", "", "output directory")
	flag.Parse()

	if *repo == "" || *out == "" {
		log.Fatal("usage: naive --repo <org/name> --out <dir> [--revision main]")
	}

	token := os.Getenv("HF_TOKEN")

	start := time.Now()
	total, n, err := run(context.Background(), *repo, *rev, *out, token)
	if err != nil {
		log.Fatalf("download failed after %d files: %v", n, err)
	}
	elapsed := time.Since(start)

	gib := float64(total) / (1 << 30)
	fmt.Printf("naive: files=%d bytes=%d (%.2f GiB) time=%.2fs throughput=%.1f MiB/s\n",
		n, total, gib, elapsed.Seconds(), float64(total)/elapsed.Seconds()/(1<<20))
}

func run(ctx context.Context, repo, rev, out, token string) (int64, int, error) {
	files, err := listTree(ctx, repo, rev, token)
	if err != nil {
		return 0, 0, fmt.Errorf("list tree: %w", err)
	}
	log.Printf("listed %d files", len(files))

	var total int64
	for i, f := range files {
		log.Printf("[%d/%d] %s (%d bytes)", i+1, len(files), f.Path, f.Size)
		n, err := fetchFile(ctx, repo, rev, f.Path, out, token)
		total += n
		if err != nil {
			return total, i, fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	return total, len(files), nil
}

// listTree paginates the Hub tree API and returns leaf (non-directory) entries.
func listTree(ctx context.Context, repo, rev, token string) ([]entry, error) {
	segments := strings.Split(repo, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	u := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", hub, strings.Join(segments, "/"), rev)

	var files []entry
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("tree %s: %s: %s", u, resp.Status, body)
		}
		var page []entry
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		for _, e := range page {
			if e.Type != "directory" && e.Type != "tree" {
				files = append(files, e)
			}
		}
		u = nextLink(resp.Header.Get("Link"))
	}
	return files, nil
}

// nextLink extracts the rel="next" URL from an RFC 8288 Link header.
func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "<")
		if i := strings.Index(part, ">"); i >= 0 {
			return part[:i]
		}
	}
	return ""
}

func fetchFile(ctx context.Context, repo, rev, path, out, token string) (int64, error) {
	dest := filepath.Join(out, path)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/%s/resolve/%s/%s", hub, repo, rev, path), nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: %s", path, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return n, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return n, err
	}
	return n, os.Rename(tmp, dest)
}
