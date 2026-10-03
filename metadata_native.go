package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
)

func jpegMetadataDates(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	return jpegDatesFrom(file)
}

// jpegDatesFrom reads Exif dates from a JPEG stream. It also serves JPEGs
// embedded in other files, such as Fujifilm RAF.
func jpegDatesFrom(file io.ReadSeeker) []string {
	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil || header[0] != 0xff || header[1] != 0xd8 {
		return nil
	}

	for {
		markerPrefix := make([]byte, 1)
		if _, err := io.ReadFull(file, markerPrefix); err != nil {
			return nil
		}
		if markerPrefix[0] != 0xff {
			continue
		}

		marker := make([]byte, 1)
		if _, err := io.ReadFull(file, marker); err != nil {
			return nil
		}
		for marker[0] == 0xff {
			if _, err := io.ReadFull(file, marker); err != nil {
				return nil
			}
		}

		if marker[0] == 0xda || marker[0] == 0xd9 {
			return nil
		}

		lengthBytes := make([]byte, 2)
		if _, err := io.ReadFull(file, lengthBytes); err != nil {
			return nil
		}
		length := int(binary.BigEndian.Uint16(lengthBytes))
		if length < 2 {
			return nil
		}
		dataLength := length - 2

		if marker[0] != 0xe1 {
			if _, err := file.Seek(int64(dataLength), io.SeekCurrent); err != nil {
				return nil
			}
			continue
		}

		data := make([]byte, dataLength)
		if _, err := io.ReadFull(file, data); err != nil {
			return nil
		}
		if bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
			return parseTIFFDates(data[6:])
		}
	}
}

func tiffLikeMetadataDates(path string) []string {
	data, err := readPrefix(path, 2*1024*1024)
	if err != nil {
		return nil
	}
	return parseTIFFDates(data)
}

func pngMetadataDates(path string) []string {
	data, err := readPrefix(path, 2*1024*1024)
	if err != nil || len(data) < 8 || !bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return nil
	}

	var candidates []string
	offset := 8
	for offset+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunkType := string(data[offset+4 : offset+8])
		chunkStart := offset + 8
		chunkEnd := chunkStart + length
		if length < 0 || chunkEnd > len(data) {
			break
		}

		switch chunkType {
		case "eXIf":
			candidates = append(candidates, parseTIFFDates(data[chunkStart:chunkEnd])...)
		case "tEXt":
			chunk := data[chunkStart:chunkEnd]
			parts := bytes.SplitN(chunk, []byte{0}, 2)
			if len(parts) == 2 && looksLikeDateKey(string(parts[0])) {
				candidates = append(candidates, string(parts[1]))
			}
		case "IDAT":
			if len(candidates) > 0 {
				return candidates
			}
		}

		offset = chunkEnd + 4
	}
	return candidates
}

func webpMetadataDates(path string) []string {
	data, err := readPrefix(path, 4*1024*1024)
	if err != nil || len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil
	}

	var candidates []string
	offset := 12
	for offset+8 <= len(data) {
		chunkType := string(data[offset : offset+4])
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		chunkStart := offset + 8
		chunkEnd := chunkStart + length
		if length < 0 || chunkEnd > len(data) {
			break
		}
		if chunkType == "EXIF" {
			candidates = append(candidates, parseTIFFDates(data[chunkStart:chunkEnd])...)
		}
		offset = chunkEnd
		if offset%2 == 1 {
			offset++
		}
	}
	return candidates
}

func looksLikeDateKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "date") || strings.Contains(key, "time") || strings.Contains(key, "creation")
}

func readPrefix(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func parseTIFFDates(data []byte) []string {
	if len(data) < 8 {
		return nil
	}

	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil
	}

	if order.Uint16(data[2:4]) != 42 {
		return nil
	}

	firstIFD := int(order.Uint32(data[4:8]))
	return parseIFDDates(data, firstIFD, order, 0)
}

func parseIFDDates(data []byte, offset int, order binary.ByteOrder, depth int) []string {
	if depth > 4 || offset < 0 || offset+2 > len(data) {
		return nil
	}

	count := int(order.Uint16(data[offset : offset+2]))
	entriesStart := offset + 2
	entriesEnd := entriesStart + count*12
	if count < 0 || entriesEnd > len(data) {
		return nil
	}

	var ifdDates []string
	var subIFDOffsets []int
	for i := 0; i < count; i++ {
		entry := data[entriesStart+i*12 : entriesStart+(i+1)*12]
		tag := order.Uint16(entry[0:2])
		fieldType := order.Uint16(entry[2:4])
		fieldCount := order.Uint32(entry[4:8])

		switch tag {
		case 0x0132, 0x9003, 0x9004:
			if fieldType != 2 {
				continue
			}
			if value, ok := tiffEntryBytes(data, entry, order, fieldType, fieldCount); ok {
				ifdDates = append(ifdDates, strings.TrimRight(string(value), "\x00 "))
			}
		case 0x8769, 0x8825:
			if pointer, ok := tiffEntryUint(entry, order, fieldType); ok {
				subIFDOffsets = append(subIFDOffsets, int(pointer))
			}
		}
	}

	var subDates []string
	for _, subOffset := range subIFDOffsets {
		subDates = append(subDates, parseIFDDates(data, subOffset, order, depth+1)...)
	}

	return append(subDates, ifdDates...)
}

func tiffEntryBytes(data, entry []byte, order binary.ByteOrder, fieldType uint16, fieldCount uint32) ([]byte, bool) {
	typeSize, ok := tiffTypeSize(fieldType)
	if !ok {
		return nil, false
	}

	total := uint64(typeSize) * uint64(fieldCount)
	if total == 0 || total > uint64(len(data)) {
		return nil, false
	}

	if total <= 4 {
		return entry[8 : 8+int(total)], true
	}

	valueOffset := int(order.Uint32(entry[8:12]))
	valueEnd := valueOffset + int(total)
	if valueOffset < 0 || valueEnd > len(data) {
		return nil, false
	}
	return data[valueOffset:valueEnd], true
}

func tiffEntryUint(entry []byte, order binary.ByteOrder, fieldType uint16) (uint32, bool) {
	switch fieldType {
	case 3:
		return uint32(order.Uint16(entry[8:10])), true
	case 4:
		return order.Uint32(entry[8:12]), true
	default:
		return 0, false
	}
}

func tiffTypeSize(fieldType uint16) (uint32, bool) {
	switch fieldType {
	case 1, 2, 6, 7:
		return 1, true
	case 3, 8:
		return 2, true
	case 4, 9, 11:
		return 4, true
	case 5, 10, 12:
		return 8, true
	default:
		return 0, false
	}
}
