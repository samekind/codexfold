//go:build cgo && (darwin || linux)

package codexzstd

/*
#include <stddef.h>

// libzstd's public API, resolved against the copy that
// github.com/DataDog/zstd v1.5.7 compiles into the binary.
typedef struct ZSTD_CCtx_s ZSTD_CCtx;
typedef struct { const void* src; size_t size; size_t pos; } ZSTD_inBuffer;
typedef struct { void* dst; size_t size; size_t pos; } ZSTD_outBuffer;
extern ZSTD_CCtx* ZSTD_createCCtx(void);
extern size_t ZSTD_freeCCtx(ZSTD_CCtx* cctx);
extern size_t ZSTD_CCtx_setParameter(ZSTD_CCtx* cctx, int param, int value);
extern size_t ZSTD_CCtx_setPledgedSrcSize(ZSTD_CCtx* cctx, unsigned long long pledgedSrcSize);
extern size_t ZSTD_compressStream2(ZSTD_CCtx* cctx, ZSTD_outBuffer* output, ZSTD_inBuffer* input, int endOp);
extern unsigned ZSTD_isError(size_t code);
extern const char* ZSTD_getErrorName(size_t code);
extern unsigned ZSTD_versionNumber(void);
extern size_t ZSTD_CStreamOutSize(void);

static size_t codexfold_stream(ZSTD_CCtx* cctx, void* dst, size_t dst_size, size_t* dst_pos,
                               const void* src, size_t src_size, size_t* src_pos, int end_op) {
	ZSTD_outBuffer output = { dst, dst_size, *dst_pos };
	ZSTD_inBuffer input = { src, src_size, *src_pos };
	size_t result = ZSTD_compressStream2(cctx, &output, &input, end_op);
	*dst_pos = output.pos;
	*src_pos = input.pos;
	return result;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"unsafe"

	_ "github.com/DataDog/zstd"
)

const (
	zstdVersion           = 10507
	zstdCompressionLevel  = 100
	zstdContentSizeFlag   = 200
	zstdChecksumFlag      = 201
	zstdNbWorkers         = 400
	zstdEndDirectiveCont  = 0
	zstdEndDirectiveEnd   = 2
	codexCompressionLevel = 3
)

func Supported() bool { return C.ZSTD_versionNumber() == zstdVersion }

type encoder struct {
	cctx   *C.ZSTD_CCtx
	w      io.Writer
	output []byte
}

func zstdError(code C.size_t) error {
	if C.ZSTD_isError(code) == 0 {
		return nil
	}
	return fmt.Errorf("zstd: %s", C.GoString(C.ZSTD_getErrorName(code)))
}

func newEncoder(w io.Writer, size int64) (*encoder, error) {
	if !Supported() {
		return nil, ErrUnsupported
	}
	if size < 0 {
		return nil, errors.New("negative zstd content size")
	}
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		return nil, errors.New("zstd: cannot allocate compression context")
	}
	e := &encoder{cctx: cctx, w: w, output: make([]byte, int(C.ZSTD_CStreamOutSize()))}
	for _, setting := range [][2]C.int{
		{zstdCompressionLevel, codexCompressionLevel},
		{zstdContentSizeFlag, 1},
		{zstdChecksumFlag, 0},
		{zstdNbWorkers, 0},
	} {
		if err := zstdError(C.ZSTD_CCtx_setParameter(cctx, setting[0], setting[1])); err != nil {
			e.abort()
			return nil, err
		}
	}
	if err := zstdError(C.ZSTD_CCtx_setPledgedSrcSize(cctx, C.ulonglong(size))); err != nil {
		e.abort()
		return nil, err
	}
	return e, nil
}

// stream feeds input (nil at the end) and writes every byte libzstd emits.
func (e *encoder) stream(input []byte, end bool) error {
	directive := C.int(zstdEndDirectiveCont)
	if end {
		directive = zstdEndDirectiveEnd
	}
	var src unsafe.Pointer
	if len(input) > 0 {
		src = unsafe.Pointer(&input[0])
	}
	var srcPos C.size_t
	for {
		var dstPos C.size_t
		remaining := C.codexfold_stream(e.cctx, unsafe.Pointer(&e.output[0]), C.size_t(len(e.output)), &dstPos,
			src, C.size_t(len(input)), &srcPos, directive)
		if err := zstdError(remaining); err != nil {
			return err
		}
		if dstPos > 0 {
			if _, err := e.w.Write(e.output[:dstPos]); err != nil {
				return err
			}
		}
		if end {
			if remaining == 0 {
				return nil
			}
		} else if int(srcPos) == len(input) {
			return nil
		}
	}
}

func (e *encoder) Write(p []byte) (int, error) {
	if e.cctx == nil {
		return 0, errors.New("zstd encoder is closed")
	}
	if err := e.stream(p, false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (e *encoder) Close() error {
	if e.cctx == nil {
		return nil
	}
	err := e.stream(nil, true)
	e.abort()
	return err
}

func (e *encoder) abort() {
	if e.cctx != nil {
		C.ZSTD_freeCCtx(e.cctx)
		e.cctx = nil
	}
}
