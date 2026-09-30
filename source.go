package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FetchRequest asks a source for one file. Validators make the fetch conditional.
type FetchRequest struct {
	Path         string // clean, relative ("products/1/a.webp")
	ETag         string
	LastModified string
	Range        string // forwarded only when streaming through
}

// FetchResult is what a source returned. Body is nil for 304 and 404.
type FetchResult struct {
	Status       int // 200, 206, 304, 404
	Body         io.ReadCloser
	Size         int64 // -1 when unknown
	ContentType  string
	ETag         string
	LastModified string
	ContentRange string
}

func (r *FetchResult) Close() {
	if r != nil && r.Body != nil {
		r.Body.Close()
	}
}

// Source is where originals come from: the production origin or a local folder.
type Source interface {
	Fetch(ctx context.Context, req FetchRequest) (*FetchResult, error)
	Describe() string
	// Probe checks the source can be reached at all.
	Probe(ctx context.Context) error
}

func NewSource(c Config) Source {
	if c.OriginURL != "" {
		return &OriginSource{
			base: c.OriginURL,
			host: c.OriginHost,
			client: &http.Client{
				Timeout: c.OriginTimeout,
				Transport: &http.Transport{
					Proxy:               http.ProxyFromEnvironment,
					MaxIdleConns:        64,
					MaxIdleConnsPerHost: 32,
					IdleConnTimeout:     90 * time.Second,
				},
				// A redirect from storage is followed like a browser would.
			},
		}
	}
	return &DiskSource{base: c.BasePath}
}

// ─── Origin (reverse proxy) ─────────────────────────────────────

type OriginSource struct {
	base   string
	host   string
	client *http.Client
}

func (o *OriginSource) Describe() string { return o.base }

// Probe asks for the storage root: any answer below 500 (usually 403/404) means the origin is up.
func (o *OriginSource) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/", nil)
	if err != nil {
		return err
	}
	if o.host != "" {
		req.Host = o.host
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("origin answered %d", resp.StatusCode)
	}
	return nil
}

func (o *OriginSource) Fetch(ctx context.Context, fr FetchRequest) (*FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/"+escapePath(fr.Path), nil)
	if err != nil {
		return nil, err
	}
	if o.host != "" {
		req.Host = o.host
	}
	req.Header.Set("User-Agent", "madan-asset-service/1")
	if fr.ETag != "" {
		req.Header.Set("If-None-Match", fr.ETag)
	}
	if fr.LastModified != "" {
		req.Header.Set("If-Modified-Since", fr.LastModified)
	}
	if fr.Range != "" {
		req.Header.Set("Range", fr.Range)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return &FetchResult{
			Status:       resp.StatusCode,
			Body:         resp.Body,
			Size:         resp.ContentLength,
			ContentType:  resp.Header.Get("Content-Type"),
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
			ContentRange: resp.Header.Get("Content-Range"),
		}, nil
	case http.StatusNotModified:
		resp.Body.Close()
		return &FetchResult{Status: http.StatusNotModified, Size: -1}, nil
	case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
		// Forbidden is what nginx returns for directories and hidden files: treat as missing.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return &FetchResult{Status: http.StatusNotFound, Size: -1}, nil
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("origin answered %d", resp.StatusCode)
	}
}

// escapePath escapes each segment but keeps the slashes.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// ─── Disk ───────────────────────────────────────────────────────

type DiskSource struct {
	base string
}

func (d *DiskSource) Describe() string { return d.base }

func (d *DiskSource) Probe(context.Context) error {
	info, err := os.Stat(d.base)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", d.base)
	}
	return nil
}

func (d *DiskSource) Fetch(_ context.Context, fr FetchRequest) (*FetchResult, error) {
	full := filepath.Join(d.base, fr.Path)
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return &FetchResult{Status: http.StatusNotFound, Size: -1}, nil
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		f.Close()
		return &FetchResult{Status: http.StatusNotFound, Size: -1}, nil
	}

	lastModified := info.ModTime().UTC().Format(http.TimeFormat)
	etag := fmt.Sprintf(`W/"%x-%x"`, info.ModTime().UnixNano(), info.Size())
	if (fr.ETag != "" && fr.ETag == etag) || (fr.ETag == "" && fr.LastModified != "" && fr.LastModified == lastModified) {
		f.Close()
		return &FetchResult{Status: http.StatusNotModified, Size: -1}, nil
	}

	res := &FetchResult{
		Status:       http.StatusOK,
		Body:         f,
		Size:         info.Size(),
		ContentType:  mime.TypeByExtension(strings.ToLower(filepath.Ext(full))),
		ETag:         etag,
		LastModified: lastModified,
	}

	if start, end, ok := parseSingleRange(fr.Range, info.Size()); ok {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		res.Status = http.StatusPartialContent
		res.Size = end - start + 1
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(f, res.Size), f}
		res.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size())
	}
	return res, nil
}

// parseSingleRange understands "bytes=a-b", "bytes=a-" and "bytes=-n". Anything else means "whole file".
func parseSingleRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") || size <= 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		if e, err := strconv.ParseInt(b, 10, 64); err == nil && e >= start && e < end {
			end = e
		}
	}
	return start, end, true
}
