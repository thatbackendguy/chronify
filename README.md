# Chronify

Chronify is a terminal-based photo and video organizer for large media
libraries. It scans recursively and organizes files into year/month folders:

```text
Media Library/
  2024/
    05/
      IMG_20240509_100322.jpg
      VID_20240509_120000.mov
  _unknown_date/
    scan-without-date.png
```

It is designed for large collections, external drives, and cautious migration
workflows. Dry-run is the default.

## Features

- Organizes photos and videos into `YYYY/MM` folders
- Scans recursively without loading media contents into memory
- Supports common image, RAW, HEIC/HEIF, and video formats
- Prioritizes media metadata before filename dates for every photo and video
- Uses embedded metadata when available for JPEG, TIFF/DNG-style files, PNG,
  and WebP
- Uses filename dates for common camera, phone, screenshot, and WhatsApp-style
  names
- Can use optional `ffprobe`, `exiftool`, and macOS `mdls` metadata when present
- Falls back to filesystem modified time by default
- Writes a CSV manifest for every dry-run and apply run
- Handles duplicates by renaming or skipping
- Moves across drives safely by falling back to copy-and-delete when needed

## Build

```bash
go build -o chronify .
```

## Recommended workflow

Start with a dry-run and inspect the CSV manifest:

```bash
./chronify \
  -source "/Volumes/Camera Dump" \
  -dest "/Volumes/Media Library"
```

Apply the organization after reviewing the report:

```bash
./chronify \
  -source "/Volumes/Camera Dump" \
  -dest "/Volumes/Media Library" \
  -apply \
  -mode move
```

For the safest first migration, copy instead of move:

```bash
./chronify \
  -source "/Volumes/Camera Dump" \
  -dest "/Volumes/Media Library" \
  -apply \
  -mode copy
```

## Useful flags

```text
-source          Folder to scan recursively
-dest            Folder where YYYY/MM folders will be created
-apply           Actually move/copy files; omitted means dry-run
-mode            move or copy when -apply is set
-media           all, images, or videos
-metadata        auto, native, or never
-modtime         Use filesystem modified time as fallback
-conflict        rename or skip when destination exists
-workers         Parallel date-detection workers
-report          CSV manifest path, or "none"
-min-year        Minimum accepted media year
-max-year        Maximum accepted media year
```

## Date detection order

1. Native embedded metadata for supported formats
2. Optional external metadata tools when `-metadata auto` is enabled
3. Date-like filename patterns such as `IMG_20250204_223211.jpg`,
   `PXL_20231203_092312345.MP.jpg`, `2024-05-09.png`, and `2024.05.mov`
4. Filesystem modified time, unless `-modtime=false`
5. `_unknown_date/` when no usable date is found

## Optional metadata tools

Chronify has no required runtime dependencies. If these tools are installed,
`-metadata auto` can use them for better coverage:

- `ffprobe` for video and HEIC/HEIF/AVIF creation timestamps
- `exiftool` for broad image/video metadata support
- `mdls` on macOS for Spotlight creation dates

## Examples

Dry-run only images from a folder:

```bash
./chronify -source ~/Pictures/import -dest ~/Pictures/library -media images
```

Move files, but skip duplicates instead of renaming them:

```bash
./chronify -source ~/Pictures/import -dest ~/Pictures/library -apply -conflict skip
```

Avoid filesystem modified-time fallback:

```bash
./chronify -source ~/Pictures/import -dest ~/Pictures/library -modtime=false
```

## License

MIT License © [ThatBackendGuy](https://github.com/thatbackendguy/)
