package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/samekind/codexfold/internal/fold"
)

type View struct {
	manifest fold.Manifest
	ends     []int64
	reader   ObjectReader
}

func NewView(manifest fold.Manifest, reader ObjectReader) (*View, error) {
	if reader == nil {
		return nil, errors.New("virtual view object reader is required")
	}
	if manifest.Version != fold.ManifestVersion || manifest.Kind != fold.ManifestKind {
		return nil, fmt.Errorf("unsupported fold manifest version=%d kind=%q", manifest.Version, manifest.Kind)
	}
	view := &View{manifest: manifest, reader: reader, ends: make([]int64, len(manifest.Parts))}
	var total int64
	for index, part := range manifest.Parts {
		if part.Kind != fold.PartResidual && part.Kind != fold.PartField {
			return nil, fmt.Errorf("manifest part %d has unsupported kind %q", index, part.Kind)
		}
		if len(part.Object.SHA256) != 64 || part.Object.RawBytes <= 0 {
			return nil, fmt.Errorf("manifest part %d has invalid object reference", index)
		}
		if part.Object.RawBytes > int64(^uint64(0)>>1)-total {
			return nil, errors.New("manifest byte length overflows int64")
		}
		total += part.Object.RawBytes
		view.ends[index] = total
	}
	if total != manifest.Source.Bytes {
		return nil, fmt.Errorf("manifest parts total %d bytes, source records %d", total, manifest.Source.Bytes)
	}
	return view, nil
}

func (v *View) Size() int64 { return v.manifest.Source.Bytes }

func (v *View) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative virtual read offset")
	}
	if len(destination) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset >= v.Size() {
		return 0, io.EOF
	}
	partIndex := sort.Search(len(v.ends), func(index int) bool { return v.ends[index] > offset })
	if partIndex == len(v.ends) {
		return 0, fmt.Errorf("manifest has no part for offset %d", offset)
	}
	written := 0
	for written < len(destination) && offset < v.Size() {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if partIndex == len(v.ends) {
			return written, fmt.Errorf("manifest has no part for offset %d", offset)
		}
		partStart := int64(0)
		if partIndex > 0 {
			partStart = v.ends[partIndex-1]
		}
		part := v.manifest.Parts[partIndex]
		inside := offset - partStart
		remaining := part.Object.RawBytes - inside
		need := len(destination) - written
		if int64(need) > remaining {
			need = int(remaining)
		}
		partDestinationStart := written
		n, err := v.reader.ReadAt(ctx, part.Object, destination[written:written+need], inside)
		if n < 0 || n > need {
			return written, fmt.Errorf("object reader returned invalid byte count %d for request %d", n, need)
		}
		written += n
		offset += int64(n)
		if n != need {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return written, fmt.Errorf("read manifest part %d: %w", partIndex, err)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return written, fmt.Errorf("read manifest part %d: %w", partIndex, err)
		}
		if inside == 0 && int64(need) == part.Object.RawBytes {
			source := destination[partDestinationStart : partDestinationStart+need]
			for written < len(destination) && partIndex+1 < len(v.manifest.Parts) {
				if err := ctx.Err(); err != nil {
					return written, err
				}
				next := v.manifest.Parts[partIndex+1]
				if !sameObjectBytes(part.Object, next.Object) {
					break
				}
				repeatedBytes := len(destination) - written
				if int64(repeatedBytes) > next.Object.RawBytes {
					repeatedBytes = int(next.Object.RawBytes)
				}
				copy(destination[written:written+repeatedBytes], source[:repeatedBytes])
				written += repeatedBytes
				offset += int64(repeatedBytes)
				partIndex++
				if int64(repeatedBytes) < next.Object.RawBytes {
					break
				}
			}
		}
		partIndex++
	}
	if written < len(destination) {
		return written, io.EOF
	}
	return written, nil
}

func sameObjectBytes(first fold.ObjectRef, second fold.ObjectRef) bool {
	return first.SHA256 == second.SHA256 && first.RawBytes == second.RawBytes
}
