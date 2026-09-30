//go:build !novips

package main

import "github.com/h2non/bimg"

const transformEnabled = true

func transformImage(data []byte, t TransformOptions) ([]byte, error) {
	options := bimg.Options{
		Width:         t.Width,
		Height:        t.Height,
		Quality:       t.Quality,
		StripMetadata: true,
		NoAutoRotate:  false,
		Interlace:     true,
		Gravity:       bimg.GravityCentre,
	}

	switch t.Fit {
	case "contain":
		options.Crop = false
	case "scale-down":
		options.Enlarge = false
	default:
		options.Crop = true
	}

	switch t.Format {
	case "webp":
		options.Type = bimg.WEBP
	case "avif":
		options.Type = bimg.AVIF
	case "png":
		options.Type = bimg.PNG
	default:
		options.Type = bimg.JPEG
	}

	return bimg.NewImage(data).Process(options)
}
