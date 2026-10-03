package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"time"
)

// ISO base media file format ("BMFF") is the box structure shared by MP4,
// QuickTime MOV, 3GP, HEIC/HEIF, AVIF and Canon CR3. These readers only look
// at box headers and the few small boxes that hold dates, using ReadAt, so a
// 4 GB video costs a handful of small reads even when its metadata sits at
// the end of the file.

const (
	maxBoxPayload  = 4 * 1024 * 1024
	appleCreateKey = "com.apple.quicktime.creationdate"
)

var (
	// Seconds between the QuickTime epoch (1904-01-01) and the Unix epoch.
	quickTimeEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	canonCR3UUID   = []byte{0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0, 0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48}
)

type bmffBox struct {
	Type  string
	UUID  []byte // set for "uuid" boxes
	Start int64  // payload start
	End   int64  // payload end
}

func (b bmffBox) size() int64 { return b.End - b.Start }

// walkBoxes calls fn for each box between start and end. Returning false
// from fn stops the walk.
func walkBoxes(r io.ReaderAt, start, end int64, fn func(bmffBox) bool) {
	header := make([]byte, 16)
	for offset := start; offset+8 <= end; {
		if _, err := r.ReadAt(header[:8], offset); err != nil {
			return
		}
		size := int64(binary.BigEndian.Uint32(header[0:4]))
		box := bmffBox{Type: string(header[4:8]), Start: offset + 8}

		switch size {
		case 0: // box runs to the end of its parent
			size = end - offset
		case 1: // 64-bit size follows the type
			if _, err := r.ReadAt(header[8:16], offset+8); err != nil {
				return
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			box.Start += 8
		}
		if size < box.Start-offset || offset+size > end || size < 0 {
			return
		}
		box.End = offset + size

		if box.Type == "uuid" {
			if box.size() < 16 {
				return
			}
			box.UUID = make([]byte, 16)
			if _, err := r.ReadAt(box.UUID, box.Start); err != nil {
				return
			}
			box.Start += 16
		}

		if !fn(box) {
			return
		}
		offset = box.End
	}
}

func findBox(r io.ReaderAt, start, end int64, boxType string) (bmffBox, bool) {
	var found bmffBox
	ok := false
	walkBoxes(r, start, end, func(b bmffBox) bool {
		if b.Type == boxType {
			found, ok = b, true
			return false
		}
		return true
	})
	return found, ok
}

func readBox(r io.ReaderAt, b bmffBox) ([]byte, bool) {
	if b.size() < 0 || b.size() > maxBoxPayload {
		return nil, false
	}
	data := make([]byte, b.size())
	if _, err := r.ReadAt(data, b.Start); err != nil {
		return nil, false
	}
	return data, true
}

func openSized(path string) (*os.File, int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, false
	}
	return file, info.Size(), true
}

// quickTimeDates returns capture dates from an MP4/MOV/3GP file, best first:
// Apple's creationdate (local time with offset), ©day, then the movie
// header's creation time (UTC).
func quickTimeDates(path string) []string {
	file, size, ok := openSized(path)
	if !ok {
		return nil
	}
	defer file.Close()

	moov, ok := findBox(file, 0, size, "moov")
	if !ok {
		return nil
	}

	var apple, day, header []string
	walkBoxes(file, moov.Start, moov.End, func(b bmffBox) bool {
		switch b.Type {
		case "mvhd":
			if t, ok := mvhdCreationTime(file, b); ok {
				header = append(header, t.Format(time.RFC3339))
			}
		case "meta":
			apple = append(apple, quickTimeMetaDates(file, b)...)
		case "udta":
			walkBoxes(file, b.Start, b.End, func(u bmffBox) bool {
				switch u.Type {
				case "\xa9day":
					day = append(day, quickTimeStringValues(file, u)...)
				case "meta":
					apple = append(apple, quickTimeMetaDates(file, u)...)
				}
				return true
			})
		}
		return true
	})

	return append(append(apple, day...), header...)
}

func mvhdCreationTime(r io.ReaderAt, b bmffBox) (time.Time, bool) {
	data := make([]byte, 12)
	if b.size() < 12 {
		return time.Time{}, false
	}
	if _, err := r.ReadAt(data, b.Start); err != nil {
		return time.Time{}, false
	}

	var seconds uint64
	if data[0] == 1 {
		seconds = binary.BigEndian.Uint64(data[4:12])
	} else {
		seconds = uint64(binary.BigEndian.Uint32(data[4:8]))
	}
	// Many encoders leave this at 0 (1904) or write a Unix-epoch value by
	// mistake; neither is a real capture date for a digital video.
	if seconds == 0 || seconds > 1<<40 {
		return time.Time{}, false
	}
	t := quickTimeEpoch.Add(time.Duration(seconds) * time.Second)
	if t.Year() < 1980 {
		return time.Time{}, false
	}
	return t, true
}

// metaChildrenStart handles both QuickTime "meta" (a plain container) and
// ISO "meta" (a full box with 4 bytes of version/flags before children).
func metaChildrenStart(r io.ReaderAt, b bmffBox) int64 {
	peek := make([]byte, 8)
	if b.size() >= 8 {
		if _, err := r.ReadAt(peek, b.Start); err == nil && string(peek[4:8]) == "hdlr" {
			return b.Start
		}
	}
	return b.Start + 4
}

// quickTimeMetaDates finds com.apple.quicktime.creationdate in a keys/ilst
// metadata box, as written by iPhones and many other phones.
func quickTimeMetaDates(r io.ReaderAt, meta bmffBox) []string {
	start := metaChildrenStart(r, meta)
	keysBox, ok := findBox(r, start, meta.End, "keys")
	if !ok {
		return nil
	}
	ilst, ok := findBox(r, start, meta.End, "ilst")
	if !ok {
		return nil
	}

	keys, ok := readBox(r, keysBox)
	if !ok || len(keys) < 8 {
		return nil
	}
	wanted := uint32(0)
	count := binary.BigEndian.Uint32(keys[4:8])
	for i, offset := uint32(1), 8; i <= count && offset+8 <= len(keys); i++ {
		keySize := int(binary.BigEndian.Uint32(keys[offset : offset+4]))
		if keySize < 8 || offset+keySize > len(keys) {
			break
		}
		if string(keys[offset+8:offset+keySize]) == appleCreateKey {
			wanted = i
			break
		}
		offset += keySize
	}
	if wanted == 0 {
		return nil
	}

	var dates []string
	walkBoxes(r, ilst.Start, ilst.End, func(item bmffBox) bool {
		if binary.BigEndian.Uint32([]byte(item.Type)) != wanted {
			return true
		}
		dates = append(dates, quickTimeStringValues(r, item)...)
		return false
	})
	return dates
}

// quickTimeStringValues reads a text value stored either in a "data" child
// box or in the classic QuickTime form (2-byte length, 2-byte language).
func quickTimeStringValues(r io.ReaderAt, b bmffBox) []string {
	if data, ok := findBox(r, b.Start, b.End, "data"); ok {
		payload, ok := readBox(r, data)
		if ok && len(payload) > 8 {
			return []string{string(payload[8:])}
		}
		return nil
	}

	payload, ok := readBox(r, b)
	if !ok || len(payload) < 4 {
		return nil
	}
	length := int(binary.BigEndian.Uint16(payload[0:2]))
	if 4+length <= len(payload) {
		return []string{string(payload[4 : 4+length])}
	}
	return []string{string(payload[4:])}
}

// heifDates reads the Exif item of a HEIC/HEIF/AVIF image.
func heifDates(path string) []string {
	file, size, ok := openSized(path)
	if !ok {
		return nil
	}
	defer file.Close()

	meta, ok := findBox(file, 0, size, "meta")
	if !ok {
		return nil
	}
	start := meta.Start + 4 // ISO meta is a full box

	iinf, ok := findBox(file, start, meta.End, "iinf")
	if !ok {
		return nil
	}
	exifID, ok := heifExifItemID(file, iinf)
	if !ok {
		return nil
	}

	iloc, ok := findBox(file, start, meta.End, "iloc")
	if !ok {
		return nil
	}
	ilocData, ok := readBox(file, iloc)
	if !ok {
		return nil
	}
	offset, length, ok := heifItemLocation(ilocData, exifID)
	if !ok || length <= 4 || offset+length > uint64(size) {
		return nil
	}
	if length > maxBoxPayload {
		length = maxBoxPayload
	}

	exif := make([]byte, length)
	if _, err := file.ReadAt(exif, int64(offset)); err != nil {
		return nil
	}
	if tiff, ok := tiffStart(exif); ok {
		return parseTIFFDates(tiff)
	}
	return nil
}

func heifExifItemID(r io.ReaderAt, iinf bmffBox) (uint32, bool) {
	header := make([]byte, 4)
	if _, err := r.ReadAt(header, iinf.Start); err != nil {
		return 0, false
	}
	start := iinf.Start + 4 + 2 // version/flags + uint16 entry count
	if header[0] != 0 {
		start += 2 // uint32 entry count
	}

	var id uint32
	found := false
	walkBoxes(r, start, iinf.End, func(b bmffBox) bool {
		if b.Type != "infe" {
			return true
		}
		data, ok := readBox(r, b)
		if !ok || len(data) < 4 {
			return true
		}
		switch version := data[0]; {
		case version == 2 && len(data) >= 12 && string(data[8:12]) == "Exif":
			id, found = uint32(binary.BigEndian.Uint16(data[4:6])), true
		case version == 3 && len(data) >= 14 && string(data[10:14]) == "Exif":
			id, found = binary.BigEndian.Uint32(data[4:8]), true
		}
		return !found
	})
	return id, found
}

// heifItemLocation returns the file offset and length of an item's first
// extent from an "iloc" box payload.
func heifItemLocation(data []byte, itemID uint32) (uint64, uint64, bool) {
	if len(data) < 8 {
		return 0, 0, false
	}
	version := data[0]
	offsetSize := int(data[4] >> 4)
	lengthSize := int(data[4] & 0x0f)
	baseOffsetSize := int(data[5] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(data[5] & 0x0f)
	}

	pos := 6
	read := func(n int) (uint64, bool) {
		if n == 0 {
			return 0, true
		}
		if n != 2 && n != 4 && n != 8 || pos+n > len(data) {
			return 0, false
		}
		var v uint64
		for _, b := range data[pos : pos+n] {
			v = v<<8 | uint64(b)
		}
		pos += n
		return v, true
	}

	countSize := 2
	if version == 2 {
		countSize = 4
	}
	itemCount, ok := read(countSize)
	if !ok {
		return 0, 0, false
	}

	for i := uint64(0); i < itemCount; i++ {
		id, ok := read(countSize)
		if !ok {
			return 0, 0, false
		}
		constructionMethod := uint64(0)
		if version == 1 || version == 2 {
			if constructionMethod, ok = read(2); !ok {
				return 0, 0, false
			}
			constructionMethod &= 0x0f
		}
		if _, ok := read(2); !ok { // data_reference_index
			return 0, 0, false
		}
		baseOffset, ok := read(baseOffsetSize)
		if !ok {
			return 0, 0, false
		}
		extentCount, ok := read(2)
		if !ok {
			return 0, 0, false
		}

		var firstOffset, firstLength uint64
		for e := uint64(0); e < extentCount; e++ {
			if _, ok := read(indexSize); !ok {
				return 0, 0, false
			}
			extentOffset, ok1 := read(offsetSize)
			extentLength, ok2 := read(lengthSize)
			if !ok1 || !ok2 {
				return 0, 0, false
			}
			if e == 0 {
				firstOffset, firstLength = extentOffset, extentLength
			}
		}

		if uint32(id) == itemID {
			// Only items stored at a file offset are supported.
			if constructionMethod != 0 || extentCount == 0 {
				return 0, 0, false
			}
			return baseOffset + firstOffset, firstLength, true
		}
	}
	return 0, 0, false
}

// tiffStart finds the TIFF header inside a HEIF Exif item, which begins with
// a 4-byte offset and often an "Exif\0\0" marker.
func tiffStart(exif []byte) ([]byte, bool) {
	skip := int(binary.BigEndian.Uint32(exif[0:4]))
	if body := exif[4:]; skip >= 0 && skip < len(body) && isTIFFHeader(body[skip:]) {
		return body[skip:], true
	}
	limit := len(exif)
	if limit > 64 {
		limit = 64
	}
	for _, magic := range [][]byte{[]byte("II*\x00"), []byte("MM\x00*")} {
		if i := bytes.Index(exif[:limit], magic); i >= 0 {
			return exif[i:], true
		}
	}
	return nil, false
}

func isTIFFHeader(data []byte) bool {
	return len(data) >= 4 && (bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")))
}

// cr3Dates reads the TIFF blocks Canon stores in a uuid box inside moov:
// CMT2 holds the Exif IFD (DateTimeOriginal), CMT1 holds IFD0 (DateTime).
func cr3Dates(path string) []string {
	file, size, ok := openSized(path)
	if !ok {
		return nil
	}
	defer file.Close()

	moov, ok := findBox(file, 0, size, "moov")
	if !ok {
		return nil
	}

	blocks := map[string][]string{}
	walkBoxes(file, moov.Start, moov.End, func(b bmffBox) bool {
		if b.Type != "uuid" || !bytes.Equal(b.UUID, canonCR3UUID) {
			return true
		}
		walkBoxes(file, b.Start, b.End, func(c bmffBox) bool {
			if c.Type == "CMT1" || c.Type == "CMT2" {
				if data, ok := readBox(file, c); ok {
					blocks[c.Type] = parseTIFFDates(data)
				}
			}
			return true
		})
		return false
	})
	return append(blocks["CMT2"], blocks["CMT1"]...)
}
