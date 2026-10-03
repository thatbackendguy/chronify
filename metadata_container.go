package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"time"
)

// rafDates reads Exif from the JPEG preview embedded in a Fujifilm RAF file.
// The header stores the JPEG's offset and length at bytes 84 and 88.
func rafDates(path string) []string {
	file, size, ok := openSized(path)
	if !ok {
		return nil
	}
	defer file.Close()

	header := make([]byte, 92)
	if _, err := file.ReadAt(header, 0); err != nil || !bytes.HasPrefix(header, []byte("FUJIFILMCCD-RAW")) {
		return nil
	}
	offset := int64(binary.BigEndian.Uint32(header[84:88]))
	length := int64(binary.BigEndian.Uint32(header[88:92]))
	if offset <= 0 || length <= 0 || offset+length > size {
		return nil
	}
	return jpegDatesFrom(io.NewSectionReader(file, offset, length))
}

// aviDates reads the recording date that cameras store in an AVI header:
// an IDIT chunk ("Mon Mar 10 12:00:00 2008") or an INFO/ICRD chunk.
func aviDates(path string) []string {
	data, err := readPrefix(path, 1024*1024)
	if err != nil || len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "AVI " {
		return nil
	}

	var idit, icrd []string
	var walk func(chunks []byte, depth int)
	walk = func(chunks []byte, depth int) {
		for offset := 0; offset+8 <= len(chunks); {
			id := string(chunks[offset : offset+4])
			length := int(binary.LittleEndian.Uint32(chunks[offset+4 : offset+8]))
			start := offset + 8
			end := start + length
			if length < 0 || end > len(chunks) {
				// The prefix may cut a LIST short; still look inside it.
				if id == "LIST" && depth < 4 && start+4 <= len(chunks) && string(chunks[start:start+4]) != "movi" {
					walk(chunks[start+4:], depth+1)
				}
				return
			}

			switch id {
			case "LIST":
				if depth < 4 && length >= 4 && string(chunks[start:start+4]) != "movi" {
					walk(chunks[start+4:end], depth+1)
				}
			case "IDIT":
				idit = append(idit, cleanChunkString(chunks[start:end]))
			case "ICRD":
				icrd = append(icrd, cleanChunkString(chunks[start:end]))
			}

			offset = end + length%2
		}
	}
	walk(data[12:], 0)
	return append(idit, icrd...)
}

func cleanChunkString(b []byte) string {
	return strings.TrimSpace(strings.Trim(string(b), "\x00"))
}

// Matroska/WebM element IDs (with their length-marker bits kept).
const (
	ebmlHeaderID   = 0x1A45DFA3
	mkvSegmentID   = 0x18538067
	mkvInfoID      = 0x1549A966
	mkvDateUTCID   = 0x4461
	mkvClusterID   = 0x1F43B675
	ebmlUnknownLen = -1
)

// mkvDates reads Segment → Info → DateUTC from a Matroska or WebM file:
// nanoseconds since 2001-01-01 UTC.
func mkvDates(path string) []string {
	data, err := readPrefix(path, 2*1024*1024)
	if err != nil {
		return nil
	}

	id, size, pos, ok := readEBMLElement(data, 0)
	if !ok || id != ebmlHeaderID || size < 0 {
		return nil
	}
	pos += int(size)

	for pos < len(data) {
		id, size, start, ok := readEBMLElement(data, pos)
		if !ok {
			return nil
		}
		end := len(data)
		if size != ebmlUnknownLen && start+int(size) < end {
			end = start + int(size)
		}

		switch id {
		case mkvSegmentID:
			if date, ok := mkvSegmentDate(data[start:end]); ok {
				return []string{date.Format(time.RFC3339Nano)}
			}
			return nil
		default:
			if size == ebmlUnknownLen {
				return nil
			}
			pos = end
		}
	}
	return nil
}

func mkvSegmentDate(segment []byte) (time.Time, bool) {
	for pos := 0; pos < len(segment); {
		id, size, start, ok := readEBMLElement(segment, pos)
		if !ok || size == ebmlUnknownLen || id == mkvClusterID {
			return time.Time{}, false
		}
		end := start + int(size)
		if end > len(segment) {
			end = len(segment)
		}

		if id == mkvInfoID {
			for p := start; p < end; {
				childID, childSize, childStart, ok := readEBMLElement(segment[:end], p)
				if !ok || childSize < 0 {
					return time.Time{}, false
				}
				if childID == mkvDateUTCID && childSize == 8 && childStart+8 <= end {
					nanos := int64(binary.BigEndian.Uint64(segment[childStart : childStart+8]))
					return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(nanos)), true
				}
				p = childStart + int(childSize)
			}
			return time.Time{}, false
		}
		pos = end
	}
	return time.Time{}, false
}

// readEBMLElement reads an element ID (marker bits kept) and data size at
// pos. It returns the data start offset; size is ebmlUnknownLen when the
// size field is all ones ("unknown", used by live-streamed files).
func readEBMLElement(data []byte, pos int) (uint64, int64, int, bool) {
	id, idLen, ok := readVint(data, pos, true)
	if !ok || idLen > 4 {
		return 0, 0, 0, false
	}
	size, sizeLen, ok := readVint(data, pos+idLen, false)
	if !ok {
		return 0, 0, 0, false
	}
	if size == (uint64(1)<<(7*sizeLen))-1 {
		return id, ebmlUnknownLen, pos + idLen + sizeLen, true
	}
	if size > uint64(len(data)) {
		// Larger than our prefix (e.g. a whole Segment); callers clamp.
		return id, int64(len(data)), pos + idLen + sizeLen, true
	}
	return id, int64(size), pos + idLen + sizeLen, true
}

func readVint(data []byte, pos int, keepMarker bool) (uint64, int, bool) {
	if pos >= len(data) || data[pos] == 0 {
		return 0, 0, false
	}
	first := data[pos]
	length := 1
	for mask := byte(0x80); first&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 || pos+length > len(data) {
		return 0, 0, false
	}

	value := uint64(first)
	if !keepMarker {
		value &= uint64(0xff >> length)
	}
	for _, b := range data[pos+1 : pos+length] {
		value = value<<8 | uint64(b)
	}
	return value, length, true
}
