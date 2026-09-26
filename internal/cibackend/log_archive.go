package cibackend

import (
	"archive/zip"
	"bytes"
	"io"
	"path"
	"strings"
)

// Standard gh downloads and expands run logs. A bounded compressed response is
// not enough: validate entry names, file types, counts and the decoded byte sum
// before returning an archive to a client. Nothing is extracted to disk here.
func ValidateLogArchive(data []byte) error {
	if len(data) > MaxBody {
		return ErrUnavailable
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ErrInvalid
	}
	if len(reader.File) > 1000 {
		return ErrUnavailable
	}
	var total int64
	seen := map[string]bool{}
	for _, file := range reader.File {
		name := file.Name
		if name == "" || len(name) > 1024 || path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") || path.Clean(name) != strings.TrimSuffix(name, "/") || seen[name] {
			return ErrInvalid
		}
		seen[name] = true
		if file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() {
			return ErrInvalid
		}
		if file.UncompressedSize64 > uint64(MaxBody) || total+int64(file.UncompressedSize64) > MaxBody {
			return ErrUnavailable
		}
		opened, e := file.Open()
		if e != nil {
			return ErrInvalid
		}
		size, e := io.Copy(io.Discard, io.LimitReader(opened, MaxBody-total+1))
		closeErr := opened.Close()
		if e != nil || closeErr != nil {
			return ErrInvalid
		}
		total += size
		if total > MaxBody {
			return ErrUnavailable
		}
	}
	return nil
}
