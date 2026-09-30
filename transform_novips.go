//go:build novips

package main

import "errors"

// Built with -tags novips (no libvips on the machine): originals are served, resizing is refused.
const transformEnabled = false

func transformImage([]byte, TransformOptions) ([]byte, error) {
	return nil, errors.New("image resizing is not available in this build (novips)")
}
