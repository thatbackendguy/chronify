package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func box(boxType string, payload ...[]byte) []byte {
	body := join(payload...)
	return join(be32(uint32(8+len(body))), []byte(boxType), body)
}

// tiffWithDate builds a little-endian TIFF whose IFD0 has DateTimeOriginal.
func tiffWithDate(date string) []byte {
	value := append([]byte(date), 0)
	ifd := join(
		[]byte{1, 0}, // one entry
		[]byte{0x03, 0x90, 2, 0}, le32(uint32(len(value))), le32(26),
		le32(0), // no next IFD
	)
	return join([]byte("II*\x00"), le32(8), ifd, value)
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mvhd(created time.Time) []byte {
	seconds := uint32(created.Sub(quickTimeEpoch) / time.Second)
	return box("mvhd", []byte{0, 0, 0, 0}, be32(seconds), be32(seconds), make([]byte, 88))
}

func expectDate(t *testing.T, path, want string) {
	t.Helper()
	cfg := testConfig()
	cfg.MetadataMode = "native"
	cfg.UseModTime = false
	info := determineDate(mediaFile{Path: path, Name: filepath.Base(path)}, cfg, metadataTools{})
	if !info.Known {
		t.Fatalf("no date found in %s", filepath.Base(path))
	}
	if got := info.Timestamp[:10]; got != want {
		t.Fatalf("got %s (source %s), want %s", info.Timestamp, info.Source, want)
	}
}

func TestQuickTimeMovieHeaderAfterMediaData(t *testing.T) {
	// Non-"faststart" files put moov after a large mdat.
	created := time.Date(2023, 8, 14, 12, 0, 0, 0, time.UTC)
	data := join(
		box("ftyp", []byte("isom"), be32(0)),
		box("mdat", make([]byte, 64*1024)),
		box("moov", mvhd(created)),
	)
	expectDate(t, writeTemp(t, "IMG_4321.MP4", data), "2023-08-14")
}

func TestQuickTimeIgnoresZeroMovieHeader(t *testing.T) {
	data := join(box("ftyp", []byte("qt  ")), box("moov", mvhd(quickTimeEpoch)))
	path := writeTemp(t, "clip.mov", data)
	if dates := quickTimeDates(path); len(dates) != 0 {
		t.Fatalf("expected no dates for a zeroed mvhd, got %v", dates)
	}
}

func TestQuickTimePrefersAppleCreationDate(t *testing.T) {
	// iPhone videos carry local time with an offset; mvhd is UTC and here
	// deliberately points at a different day.
	key := []byte(appleCreateKey)
	keys := box("keys", []byte{0, 0, 0, 0}, be32(1), be32(uint32(8+len(key))), []byte("mdta"), key)
	value := []byte("2024-12-31T23:30:00-0500")
	item := join(be32(uint32(8+16+len(value))), be32(1), box("data", be32(1), be32(0), value))
	meta := box("meta", box("hdlr", make([]byte, 24)), keys, box("ilst", item))

	data := join(
		box("ftyp", []byte("qt  ")),
		box("moov", mvhd(time.Date(2025, 1, 1, 4, 30, 0, 0, time.UTC)), meta),
	)
	expectDate(t, writeTemp(t, "IMG_0042.MOV", data), "2024-12-31")
}

func TestQuickTimeDayAtom(t *testing.T) {
	value := []byte("2022-06-01T09:15:00+0200")
	day := box("\xa9day", be16(uint16(len(value))), be16(0), value)
	data := join(box("ftyp", []byte("mp42")), box("moov", box("udta", day)))
	expectDate(t, writeTemp(t, "VID0001.mp4", data), "2022-06-01")
}

func TestHEICExifItem(t *testing.T) {
	exif := join(be32(6), []byte("Exif\x00\x00"), tiffWithDate("2021:03:04 05:06:07"))

	build := func(exifOffset uint32) []byte {
		infe := box("infe", []byte{2, 0, 0, 0}, be16(1), be16(0), []byte("Exif"), []byte{0})
		iinf := box("iinf", []byte{0, 0, 0, 0}, be16(1), infe)
		iloc := box("iloc", []byte{0, 0, 0, 0}, []byte{0x44, 0x00}, be16(1),
			be16(1), be16(0), be16(1), be32(exifOffset), be32(uint32(len(exif))))
		meta := box("meta", []byte{0, 0, 0, 0}, box("hdlr", make([]byte, 24)), iinf, iloc)
		return join(box("ftyp", []byte("heic"), be32(0)), meta)
	}

	// Build once to learn where mdat's payload will start.
	head := build(0)
	offset := uint32(len(head) + 8)
	data := join(build(offset), box("mdat", exif))
	expectDate(t, writeTemp(t, "IMG_1234.HEIC", data), "2021-03-04")
}

func TestCR3CanonMetadata(t *testing.T) {
	uuid := join(be32(uint32(8+16+8+len(tiffWithDate("2020:02:29 10:00:00")))), []byte("uuid"), canonCR3UUID,
		box("CMT2", tiffWithDate("2020:02:29 10:00:00")))
	data := join(box("ftyp", []byte("crx "), be32(0)), box("moov", uuid))
	expectDate(t, writeTemp(t, "_MG_0001.CR3", data), "2020-02-29")
}

func TestRAFEmbeddedJPEG(t *testing.T) {
	tiff := tiffWithDate("2019:11:05 18:00:00")
	app1 := join([]byte{0xff, 0xe1}, be16(uint16(2+6+len(tiff))), []byte("Exif\x00\x00"), tiff)
	jpeg := join([]byte{0xff, 0xd8}, app1, []byte{0xff, 0xd9})

	header := make([]byte, 100)
	copy(header, "FUJIFILMCCD-RAW 0201")
	binary.BigEndian.PutUint32(header[84:88], uint32(len(header)))
	binary.BigEndian.PutUint32(header[88:92], uint32(len(jpeg)))
	expectDate(t, writeTemp(t, "DSCF0001.RAF", join(header, jpeg)), "2019-11-05")
}

func TestAVIRecordingDate(t *testing.T) {
	riffChunk := func(id string, payload []byte) []byte {
		chunk := join([]byte(id), le32(uint32(len(payload))), payload)
		if len(payload)%2 == 1 {
			chunk = append(chunk, 0)
		}
		return chunk
	}
	hdrl := riffChunk("LIST", join([]byte("hdrl"), riffChunk("avih", make([]byte, 56)), riffChunk("IDIT", []byte("MON MAR 10 12:00:00 2008\n\x00"))))
	body := join([]byte("AVI "), hdrl, riffChunk("LIST", join([]byte("movi"), make([]byte, 32))))
	data := join([]byte("RIFF"), le32(uint32(len(body))), body)
	expectDate(t, writeTemp(t, "MVI_0001.AVI", data), "2008-03-10")
}

func TestMatroskaDateUTC(t *testing.T) {
	element := func(id []byte, payload []byte) []byte {
		return join(id, []byte{0x80 | byte(len(payload))}, payload)
	}
	nanos := time.Date(2018, 7, 1, 15, 0, 0, 0, time.UTC).Sub(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	date := make([]byte, 8)
	binary.BigEndian.PutUint64(date, uint64(nanos))

	info := element([]byte{0x15, 0x49, 0xA9, 0x66}, element([]byte{0x44, 0x61}, date))
	header := element([]byte{0x1A, 0x45, 0xDF, 0xA3}, element([]byte{0x42, 0x82}, []byte("webm")))
	// Unknown-size segment, as written by live recorders.
	segment := join([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, info)
	expectDate(t, writeTemp(t, "screen-recording.webm", join(header, segment)), "2018-07-01")
}

func TestNativeReadersIgnoreGarbage(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xff, 0x00, 0x13, 0x37}, 4096)
	for ext := range nativeReaders {
		path := writeTemp(t, "junk"+ext, garbage)
		if dates := nativeReaders[ext].read(path); len(dates) != 0 {
			t.Fatalf("%s: expected no dates from garbage, got %v", ext, dates)
		}
	}
}

func TestNativeMetadataBeatsFilenameWithoutDate(t *testing.T) {
	// The point of native readers: IMG_1234.MOV has no date in its name and
	// no exiftool/ffprobe is available.
	data := join(box("ftyp", []byte("qt  ")), box("moov", mvhd(time.Date(2024, 2, 10, 12, 0, 0, 0, time.UTC))))
	path := writeTemp(t, "IMG_1234.MOV", data)

	cfg := testConfig()
	cfg.MetadataMode = "auto"
	info := determineDate(mediaFile{Path: path, Name: "IMG_1234.MOV", MediaType: mediaVideo}, cfg, metadataTools{})
	if info.Source != "video-metadata" {
		t.Fatalf("got source %q, want video-metadata", info.Source)
	}
}
