package main

import (
	"fmt"
	"strings"
	"unsafe"
)

/*
#cgo LDFLAGS: -L. -lmoonshine

#include <stdlib.h>
#include <string.h>
#include "moonshine-c-api.h"
*/
import "C"

type MoonshineError C.int32_t
type Transcriber C.int32_t
type Stream C.int32_t

func (err MoonshineError) Error() string {
	s := C.moonshine_error_to_string(C.int32_t(err))
	if s == nil {
		return fmt.Sprintf("moonshine error %v", int32(err))
	}
	return C.GoString(s)
}

func CreateTranscriber(model string, architecture string) (Transcriber, error) {
	var arch C.uint32_t
	switch strings.ToLower(architecture) {
	case "tiny":
		arch = C.MOONSHINE_MODEL_ARCH_TINY
	case "base":
		arch = C.MOONSHINE_MODEL_ARCH_BASE
	case "tiny-streaiming":
		arch = C.MOONSHINE_MODEL_ARCH_TINY_STREAMING
	case "small-streaming":
		arch = C.MOONSHINE_MODEL_ARCH_SMALL_STREAMING
	case "medium-streaming":
		arch = C.MOONSHINE_MODEL_ARCH_MEDIUM_STREAMING
	}
	m := C.CString(model)
	defer C.free(unsafe.Pointer(m))
	handle := C.moonshine_load_transcriber_from_files(
		m, arch, nil, 0, C.MOONSHINE_HEADER_VERSION,
	)
	if handle < 0 {
		return 0, MoonshineError(handle)
	}
	return Transcriber(handle), nil
}

func DestroyTranscriber(t Transcriber) {
	C.moonshine_free_transcriber(C.int32_t(t))
}

func CreateStream(t Transcriber) (Stream, error) {
	s := C.moonshine_create_stream(C.int32_t(t), 0)
	if s < 0 {
		return 0, MoonshineError(s)
	}
	return Stream(s), nil
}

func DestroyStream(t Transcriber, s Stream) {
	C.moonshine_free_stream(C.int32_t(t), C.int32_t(s))
}

func StartStream(t Transcriber, s Stream) error {
	rc := C.moonshine_start_stream(C.int32_t(t), C.int32_t(s))
	if rc < 0 {
		return MoonshineError(s)
	}
	return nil
}

func StopStream(t Transcriber, s Stream) (string, error) {
	C.moonshine_stop_stream(C.int32_t(t), C.int32_t(s))
	return Transcribe(t, s)
}

func AddAudio(t Transcriber, s Stream, data []float32, rate int) error {
	rc := C.moonshine_transcribe_add_audio_to_stream(
		C.int32_t(t), C.int32_t(s),
		(*C.float)(unsafe.Pointer(&data[0])), C.uint64_t(len(data)),
		C.int32_t(rate), 0)
	if rc < 0 {
		return MoonshineError(rc)
	}
	return nil
}

func Transcribe(t Transcriber, s Stream) (string, error) {
	var transcript *C.struct_transcript_t
	rc := C.moonshine_transcribe_stream(
		C.int32_t(t), C.int32_t(s), 0, &transcript,
	)
	if rc < 0 {
		return "", MoonshineError(rc)
	}
	lines := unsafe.Slice(transcript.lines, transcript.line_count)
	var b strings.Builder
	first := true
	for i, l := range lines {
		if l.is_updated == 0 {
			continue
		}
		text := C.GoBytes(
			unsafe.Pointer(l.text), C.int(C.strlen(l.text)),
		)
		if i == len(lines) - 1 && l.is_complete == 0 {
			break
		}
		if !first {
			b.WriteString("\n")
		}
		b.Write(text)
		first = false
	}
	return b.String(), nil
}
