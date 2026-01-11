# Asset Service – Image Resizing & Serving

Simple, fast image processing & serving service with on-the-fly resizing, WebP/AVIF support and caching.

## Features

- Serves original images from disk
- On-the-fly resizing (?w= & ?h=)
- Quality control (?q=1..100)
- Output formats: jpeg, webp, avif, png (?format=...)
- Fit modes: cover, contain, scale-down (?fit=...)
- In-memory LRU cache + ETag + long Cache-Control
- Path traversal protection

## Requirements

- Go 1.22+
- libvips 8.10+ (very fast image processing library)

## Dependencies Installation (Ubuntu/Debian)

```bash
sudo apt-get update
sudo apt-get install -y libvips-dev pkg-config
```

macOS:
```bash
brew install libvips
```

## Docker (recommended for production)

```bash
docker build -t asset-service .
docker run -p 8080:8080 \
  -v /path/to/your/storage:/storage \
  -e BASE_PATH=/storage \
  asset-service
```

## Environment Variables

```bash
PORT=:8080
BASE_PATH=/home/user/storage/public
CACHE_SIZE=2048           # number of cached images
CACHE_TTL_HOURS=24
```

## Example URLs

```
GET /asset/products/shoe.jpg
GET /asset/products/shoe.jpg?w=800&h=600&q=80
GET /asset/products/shoe.jpg?w=400&h=400&fit=contain&format=webp
GET /asset/products/shoe.jpg?w=1200&format=avif
```

## Important Notes

- Cache key includes all transformation parameters → different sizes/formats are cached separately
- Very large images or many concurrent resizes → monitor memory
