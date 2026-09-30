package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// Image extensions libvips can resize.
var supportedImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".avif": true,
	".heic": true, ".heif": true, ".tiff": true, ".tif": true, ".gif": true,
}

const maxDimension = 4000

// TransformOptions is a resize request (?w=&h=&q=&format=&fit=).
type TransformOptions struct {
	Width, Height, Quality int
	Format, Fit            string
}

// Key is the stable, readable name of a variant, used in cache keys and the dashboard.
func (t TransformOptions) Key() string {
	return fmt.Sprintf("w=%d,h=%d,q=%d,%s,%s", t.Width, t.Height, t.Quality, t.Format, t.Fit)
}

func (t TransformOptions) ContentType() string {
	switch t.Format {
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "png":
		return "image/png"
	default:
		return "image/jpeg"
	}
}

// parseTransform reports whether the request asks for a resized image of a file that can be resized.
// Without any transform parameter the original is served untouched.
func parseTransform(q url.Values, path string) (TransformOptions, bool) {
	if !q.Has("w") && !q.Has("h") && !q.Has("q") && !q.Has("format") && !q.Has("fit") {
		return TransformOptions{}, false
	}
	if !supportedImageExts[strings.ToLower(filepath.Ext(path))] {
		return TransformOptions{}, false
	}

	t := TransformOptions{Quality: 85, Format: "jpeg", Fit: "cover"}
	t.Width = clampDimension(q.Get("w"))
	t.Height = clampDimension(q.Get("h"))
	if qi, err := strconv.Atoi(q.Get("q")); err == nil && qi >= 1 && qi <= 100 {
		t.Quality = qi
	}
	switch f := q.Get("format"); f {
	case "webp", "avif", "png", "jpeg":
		t.Format = f
	case "jpg":
		t.Format = "jpeg"
	}
	switch f := q.Get("fit"); f {
	case "contain", "scale-down":
		t.Fit = f
	}
	return t, true
}

func clampDimension(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return min(n, maxDimension)
}
