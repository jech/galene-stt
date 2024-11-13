package opus

import (
	"fmt"
	"unsafe"
)

/*
#cgo pkg-config: opus
#include <opus.h>
*/
import "C"

type Decoder struct {
	decoder        *C.OpusDecoder
	rate, channels int
}

type Error C.int

func (err Error) Error() string {
	return fmt.Sprintf("libopus: %v",
		C.GoString(C.opus_strerror(C.int(err))),
	)
}

func NewDecoder(rate int, channels int) (*Decoder, error) {
	var error C.int
	decoder := C.opus_decoder_create(
		C.opus_int32(rate),
		C.int(channels),
		&error,
	)
	if decoder == nil {
		return nil, Error(error)
	}
	return &Decoder{
		decoder:  decoder,
		rate:     rate,
		channels: channels,
	}, nil
}

func (decoder *Decoder) Destroy() {
	C.opus_decoder_destroy(decoder.decoder)
	decoder.decoder = nil
}

func (decoder *Decoder) DecodeFloat(data []byte, pcm []float32, fec bool) (int, error) {
	fecflag := C.int(0)
	if fec {
		fecflag = C.int(1)
	}

	rc := C.opus_decode_float(
		decoder.decoder,
		(*C.uchar)(unsafe.SliceData(data)),
		C.opus_int32(len(data)),
		(*C.float)(unsafe.SliceData(pcm)),
		C.int(len(pcm)/decoder.channels),
		fecflag,
	)
	if rc < 0 {
		return 0, Error(rc)
	}
	return int(rc), nil

}
